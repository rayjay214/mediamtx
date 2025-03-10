package core

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/externalcmd"
	"github.com/bluenviron/mediamtx/internal/logger"
)

func pathConfCanBeUpdated(oldPathConf *conf.Path, newPathConf *conf.Path) bool {
	clone := oldPathConf.Clone()

	clone.Record = newPathConf.Record

	clone.RPICameraBrightness = newPathConf.RPICameraBrightness
	clone.RPICameraContrast = newPathConf.RPICameraContrast
	clone.RPICameraSaturation = newPathConf.RPICameraSaturation
	clone.RPICameraSharpness = newPathConf.RPICameraSharpness
	clone.RPICameraExposure = newPathConf.RPICameraExposure
	clone.RPICameraAWB = newPathConf.RPICameraAWB
	clone.RPICameraDenoise = newPathConf.RPICameraDenoise
	clone.RPICameraShutter = newPathConf.RPICameraShutter
	clone.RPICameraMetering = newPathConf.RPICameraMetering
	clone.RPICameraGain = newPathConf.RPICameraGain
	clone.RPICameraEV = newPathConf.RPICameraEV
	clone.RPICameraFPS = newPathConf.RPICameraFPS

	return newPathConf.Equal(clone)
}

func getConfForPath(pathConfs map[string]*conf.Path, name string) (string, *conf.Path, []string, error) {
	err := conf.IsValidPathName(name)
	if err != nil {
		return "", nil, nil, fmt.Errorf("invalid path name: %s (%s)", err, name)
	}

	// normal path
	if pathConf, ok := pathConfs[name]; ok {
		return name, pathConf, nil, nil
	}

	// regular expression-based path
	for pathConfName, pathConf := range pathConfs {
		if pathConf.Regexp != nil {
			m := pathConf.Regexp.FindStringSubmatch(name)
			if m != nil {
				return pathConfName, pathConf, m, nil
			}
		}
	}

	return "", nil, nil, fmt.Errorf("path '%s' is not configured", name)
}

type pathManagerHLSManager interface {
	pathReady(*path)
	pathNotReady(*path)
}

type pathManagerParent interface {
	logger.Writer
}

type pathManager struct {
	externalAuthenticationURL string
	rtspAddress               string
	authMethods               conf.AuthMethods
	readTimeout               conf.StringDuration
	writeTimeout              conf.StringDuration
	writeQueueSize            int
	udpMaxPayloadSize         int
	pathConfs                 map[string]*conf.Path
	externalCmdPool           *externalcmd.Pool
	metrics                   *metrics
	parent                    pathManagerParent

	ctx         context.Context
	ctxCancel   func()
	wg          sync.WaitGroup
	hlsManager  pathManagerHLSManager
	paths       map[string]*path
	pathsByConf map[string]map[*path]struct{}

	// in
	chReloadConf     chan map[string]*conf.Path
	chSetHLSManager  chan pathManagerHLSManager
	chClosePath      chan *path
	chPathReady      chan *path
	chPathNotReady   chan *path
	chGetConfForPath chan pathGetConfForPathReq
	chDescribe       chan pathDescribeReq
	chAddReader      chan pathAddReaderReq
	chAddPublisher   chan pathAddPublisherReq
	chAPIPathsList   chan pathAPIPathsListReq
	chAPIPathsGet    chan pathAPIPathsGetReq
}

func newPathManager(
	externalAuthenticationURL string,
	rtspAddress string,
	authMethods conf.AuthMethods,
	readTimeout conf.StringDuration,
	writeTimeout conf.StringDuration,
	writeQueueSize int,
	udpMaxPayloadSize int,
	pathConfs map[string]*conf.Path,
	externalCmdPool *externalcmd.Pool,
	metrics *metrics,
	parent pathManagerParent,
) *pathManager {
	ctx, ctxCancel := context.WithCancel(context.Background())

	pm := &pathManager{
		externalAuthenticationURL: externalAuthenticationURL,
		rtspAddress:               rtspAddress,
		authMethods:               authMethods,
		readTimeout:               readTimeout,
		writeTimeout:              writeTimeout,
		writeQueueSize:            writeQueueSize,
		udpMaxPayloadSize:         udpMaxPayloadSize,
		pathConfs:                 pathConfs,
		externalCmdPool:           externalCmdPool,
		metrics:                   metrics,
		parent:                    parent,
		ctx:                       ctx,
		ctxCancel:                 ctxCancel,
		paths:                     make(map[string]*path),
		pathsByConf:               make(map[string]map[*path]struct{}),
		chReloadConf:              make(chan map[string]*conf.Path),
		chSetHLSManager:           make(chan pathManagerHLSManager),
		chClosePath:               make(chan *path),
		chPathReady:               make(chan *path),
		chPathNotReady:            make(chan *path),
		chGetConfForPath:          make(chan pathGetConfForPathReq),
		chDescribe:                make(chan pathDescribeReq),
		chAddReader:               make(chan pathAddReaderReq),
		chAddPublisher:            make(chan pathAddPublisherReq),
		chAPIPathsList:            make(chan pathAPIPathsListReq),
		chAPIPathsGet:             make(chan pathAPIPathsGetReq),
	}

	for pathConfName, pathConf := range pm.pathConfs {
		if pathConf.Regexp == nil {
			pm.createPath(pathConfName, pathConf, pathConfName, nil)
		}
	}

	if pm.metrics != nil {
		pm.metrics.pathManagerSet(pm)
	}

	pm.Log(logger.Debug, "path manager created")

	pm.wg.Add(1)
	go pm.run()

	return pm
}

func (pm *pathManager) close() {
	pm.Log(logger.Debug, "path manager is shutting down")
	pm.ctxCancel()
	pm.wg.Wait()
}

// Log is the main logging function.
func (pm *pathManager) Log(level logger.Level, format string, args ...interface{}) {
	pm.parent.Log(level, format, args...)
}

func (pm *pathManager) run() {
	defer pm.wg.Done()

outer:
	for {
		select {
		case newPaths := <-pm.chReloadConf:
			pm.doReloadConf(newPaths)

		case m := <-pm.chSetHLSManager:
			pm.doSetHLSManager(m)

		case pa := <-pm.chClosePath:
			pm.doClosePath(pa)

		case pa := <-pm.chPathReady:
			pm.doPathReady(pa)

		case pa := <-pm.chPathNotReady:
			pm.doPathNotReady(pa)

		case req := <-pm.chGetConfForPath:
			pm.doGetConfForPath(req)

		case req := <-pm.chDescribe:
			pm.doDescribe(req)

		case req := <-pm.chAddReader:
			pm.doAddReader(req)

		case req := <-pm.chAddPublisher:
			pm.doAddPublisher(req)

		case req := <-pm.chAPIPathsList:
			pm.doAPIPathsList(req)

		case req := <-pm.chAPIPathsGet:
			pm.doAPIPathsGet(req)

		case <-pm.ctx.Done():
			break outer
		}
	}

	pm.ctxCancel()

	if pm.metrics != nil {
		pm.metrics.pathManagerSet(nil)
	}
}

func (pm *pathManager) doReloadConf(newPaths map[string]*conf.Path) {
	for confName, pathConf := range pm.pathConfs {
		if newPath, ok := newPaths[confName]; ok {
			// configuration has changed
			if !newPath.Equal(pathConf) {
				if pathConfCanBeUpdated(pathConf, newPath) { // paths associated with the configuration can be updated
					for pa := range pm.pathsByConf[confName] {
						go pa.reloadConf(newPath)
					}
				} else { // paths associated with the configuration must be recreated
					for pa := range pm.pathsByConf[confName] {
						pm.removePath(pa)
						pa.close()
						pa.wait() // avoid conflicts between sources
					}
				}
			}
		} else {
			// configuration has been deleted, remove associated paths
			for pa := range pm.pathsByConf[confName] {
				pm.removePath(pa)
				pa.close()
				pa.wait() // avoid conflicts between sources
			}
		}
	}

	pm.pathConfs = newPaths

	// add new paths
	for pathConfName, pathConf := range pm.pathConfs {
		if _, ok := pm.paths[pathConfName]; !ok && pathConf.Regexp == nil {
			pm.createPath(pathConfName, pathConf, pathConfName, nil)
		}
	}
}

func (pm *pathManager) doSetHLSManager(m pathManagerHLSManager) {
	pm.hlsManager = m
}

func (pm *pathManager) doClosePath(pa *path) {
	if pmpa, ok := pm.paths[pa.name]; !ok || pmpa != pa {
		return
	}
	pm.removePath(pa)
}

func (pm *pathManager) doPathReady(pa *path) {
	if pm.hlsManager != nil {
		pm.hlsManager.pathReady(pa)
	}
}

func (pm *pathManager) doPathNotReady(pa *path) {
	if pm.hlsManager != nil {
		pm.hlsManager.pathNotReady(pa)
	}
}

func (pm *pathManager) doGetConfForPath(req pathGetConfForPathReq) {
	_, pathConf, _, err := getConfForPath(pm.pathConfs, req.accessRequest.name)
	if err != nil {
		req.res <- pathGetConfForPathRes{err: err}
		return
	}

	err = doAuthentication(pm.externalAuthenticationURL, pm.authMethods,
		pathConf, req.accessRequest)
	if err != nil {
		req.res <- pathGetConfForPathRes{err: err}
		return
	}

	req.res <- pathGetConfForPathRes{conf: pathConf}
}

func (pm *pathManager) doDescribe(req pathDescribeReq) {
	pathConfName, pathConf, pathMatches, err := getConfForPath(pm.pathConfs, req.accessRequest.name)
	if err != nil {
		req.res <- pathDescribeRes{err: err}
		return
	}

	err = doAuthentication(pm.externalAuthenticationURL, pm.authMethods,
		pathConf, req.accessRequest)
	if err != nil {
		req.res <- pathDescribeRes{err: err}
		return
	}

	// create path if it doesn't exist
	if _, ok := pm.paths[req.accessRequest.name]; !ok {
		pm.createPath(pathConfName, pathConf, req.accessRequest.name, pathMatches)
	}

	req.res <- pathDescribeRes{path: pm.paths[req.accessRequest.name]}
}

func (pm *pathManager) doAddReader(req pathAddReaderReq) {
	pathConfName, pathConf, pathMatches, err := getConfForPath(pm.pathConfs, req.accessRequest.name)
	if err != nil {
		req.res <- pathAddReaderRes{err: err}
		return
	}

	if !req.accessRequest.skipAuth {
		err = doAuthentication(pm.externalAuthenticationURL, pm.authMethods,
			pathConf, req.accessRequest)
		if err != nil {
			req.res <- pathAddReaderRes{err: err}
			return
		}
	}

	// create path if it doesn't exist
	if _, ok := pm.paths[req.accessRequest.name]; !ok {
		pm.createPath(pathConfName, pathConf, req.accessRequest.name, pathMatches)
	}

	req.res <- pathAddReaderRes{path: pm.paths[req.accessRequest.name]}
}

func (pm *pathManager) doAddPublisher(req pathAddPublisherReq) {
	pathConfName, pathConf, pathMatches, err := getConfForPath(pm.pathConfs, req.accessRequest.name)
	if err != nil {
		req.res <- pathAddPublisherRes{err: err}
		return
	}

	if !req.accessRequest.skipAuth {
		err = doAuthentication(pm.externalAuthenticationURL, pm.authMethods,
			pathConf, req.accessRequest)
		if err != nil {
			req.res <- pathAddPublisherRes{err: err}
			return
		}
	}

	// create path if it doesn't exist
	if _, ok := pm.paths[req.accessRequest.name]; !ok {
		pm.createPath(pathConfName, pathConf, req.accessRequest.name, pathMatches)
	}

	req.res <- pathAddPublisherRes{path: pm.paths[req.accessRequest.name]}
}

func (pm *pathManager) doAPIPathsList(req pathAPIPathsListReq) {
	paths := make(map[string]*path)

	for name, pa := range pm.paths {
		paths[name] = pa
	}

	req.res <- pathAPIPathsListRes{paths: paths}
}

func (pm *pathManager) doAPIPathsGet(req pathAPIPathsGetReq) {
	path, ok := pm.paths[req.name]
	if !ok {
		req.res <- pathAPIPathsGetRes{err: errAPINotFound}
		return
	}

	req.res <- pathAPIPathsGetRes{path: path}
}

func (pm *pathManager) createPath(
	pathConfName string,
	pathConf *conf.Path,
	name string,
	matches []string,
) {
	pa := newPath(
		pm.ctx,
		pm.rtspAddress,
		pm.readTimeout,
		pm.writeTimeout,
		pm.writeQueueSize,
		pm.udpMaxPayloadSize,
		pathConfName,
		pathConf,
		name,
		matches,
		&pm.wg,
		pm.externalCmdPool,
		pm)

	pm.paths[name] = pa

	if _, ok := pm.pathsByConf[pathConfName]; !ok {
		pm.pathsByConf[pathConfName] = make(map[*path]struct{})
	}
	pm.pathsByConf[pathConfName][pa] = struct{}{}
}

func (pm *pathManager) removePath(pa *path) {
	delete(pm.pathsByConf[pa.confName], pa)
	if len(pm.pathsByConf[pa.confName]) == 0 {
		delete(pm.pathsByConf, pa.confName)
	}
	delete(pm.paths, pa.name)
}

// confReload is called by core.
func (pm *pathManager) confReload(pathConfs map[string]*conf.Path) {
	select {
	case pm.chReloadConf <- pathConfs:
	case <-pm.ctx.Done():
	}
}

// pathReady is called by path.
func (pm *pathManager) pathReady(pa *path) {
	select {
	case pm.chPathReady <- pa:
	case <-pm.ctx.Done():
	case <-pa.ctx.Done(): // in case pathManager is blocked by path.wait()
	}
}

// pathNotReady is called by path.
func (pm *pathManager) pathNotReady(pa *path) {
	select {
	case pm.chPathNotReady <- pa:
	case <-pm.ctx.Done():
	case <-pa.ctx.Done(): // in case pathManager is blocked by path.wait()
	}
}

// closePath is called by path.
func (pm *pathManager) closePath(pa *path) {
	select {
	case pm.chClosePath <- pa:
	case <-pm.ctx.Done():
	case <-pa.ctx.Done(): // in case pathManager is blocked by path.wait()
	}
}

// getConfForPath is called by a reader or publisher.
func (pm *pathManager) getConfForPath(req pathGetConfForPathReq) pathGetConfForPathRes {
	req.res = make(chan pathGetConfForPathRes)
	select {
	case pm.chGetConfForPath <- req:
		return <-req.res

	case <-pm.ctx.Done():
		return pathGetConfForPathRes{err: fmt.Errorf("terminated")}
	}
}

// describe is called by a reader or publisher.
func (pm *pathManager) describe(req pathDescribeReq) pathDescribeRes {
	req.res = make(chan pathDescribeRes)
	select {
	case pm.chDescribe <- req:
		res1 := <-req.res
		if res1.err != nil {
			return res1
		}

		res2 := res1.path.describe(req)
		if res2.err != nil {
			return res2
		}

		res2.path = res1.path
		return res2

	case <-pm.ctx.Done():
		return pathDescribeRes{err: fmt.Errorf("terminated")}
	}
}

// addPublisher is called by a publisher.
func (pm *pathManager) addPublisher(req pathAddPublisherReq) pathAddPublisherRes {
	req.res = make(chan pathAddPublisherRes)
	select {
	case pm.chAddPublisher <- req:
		res := <-req.res
		if res.err != nil {
			return res
		}

		return res.path.addPublisher(req)

	case <-pm.ctx.Done():
		return pathAddPublisherRes{err: fmt.Errorf("terminated")}
	}
}

// addReader is called by a reader.
func (pm *pathManager) addReader(req pathAddReaderReq) pathAddReaderRes {
	req.res = make(chan pathAddReaderRes)
	select {
	case pm.chAddReader <- req:
		res := <-req.res
		if res.err != nil {
			return res
		}

		return res.path.addReader(req)

	case <-pm.ctx.Done():
		return pathAddReaderRes{err: fmt.Errorf("terminated")}
	}
}

// setHLSManager is called by hlsManager.
func (pm *pathManager) setHLSManager(s pathManagerHLSManager) {
	select {
	case pm.chSetHLSManager <- s:
	case <-pm.ctx.Done():
	}
}

// apiPathsList is called by api.
func (pm *pathManager) apiPathsList() (*apiPathList, error) {
	req := pathAPIPathsListReq{
		res: make(chan pathAPIPathsListRes),
	}

	select {
	case pm.chAPIPathsList <- req:
		res := <-req.res

		res.data = &apiPathList{
			Items: []*apiPath{},
		}

		for _, pa := range res.paths {
			item, err := pa.apiPathsGet(pathAPIPathsGetReq{})
			if err == nil {
				res.data.Items = append(res.data.Items, item)
			}
		}

		sort.Slice(res.data.Items, func(i, j int) bool {
			return res.data.Items[i].Name < res.data.Items[j].Name
		})

		return res.data, nil

	case <-pm.ctx.Done():
		return nil, fmt.Errorf("terminated")
	}
}

// apiPathsGet is called by api.
func (pm *pathManager) apiPathsGet(name string) (*apiPath, error) {
	req := pathAPIPathsGetReq{
		name: name,
		res:  make(chan pathAPIPathsGetRes),
	}

	select {
	case pm.chAPIPathsGet <- req:
		res := <-req.res
		if res.err != nil {
			return nil, res.err
		}

		data, err := res.path.apiPathsGet(req)
		return data, err

	case <-pm.ctx.Done():
		return nil, fmt.Errorf("terminated")
	}
}
