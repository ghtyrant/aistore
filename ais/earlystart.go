// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"net/url"
	"os"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/cmn/jsp"
)

const minPidConfirmations = 3

const (
	metaction1 = "early-start-have-registrations"
	metaction2 = "primary-started-up"
	metaction3 = "primary-startup-resume-rebalance"
)

type (
	bmds  map[*cluster.Snode]*bucketMD
	smaps map[*cluster.Snode]*smapX
)

// Background:
// 	- Each proxy/gateway stores a local copy of the cluster map (Smap)
// 	- Each Smap instance is versioned; the versioning is monotonic (increasing)
// 	- Only the primary (leader) proxy distributes Smap updates to all other clustered nodes
// 	- Bootstrap sequence includes /steps/ intended to resolve all the usual conflicts that may arise.
func (p *proxyrunner) bootstrap() {
	var (
		config          = cmn.GCO.Get()
		pid, primaryURL string
		cii             *clusterInfo
		primary         bool
	)

	// 1: load a local copy and try to utilize it for discovery
	smap, reliable := p.tryLoadSmap()
	if !reliable {
		smap = nil
	} else {
		glog.Infof("%s: loaded %s", p.si, smap.StringEx())
	}

	// 2. make the preliminary/primary decision
	pid, primary = p.determineRole(smap)

	// 3. primary?
	if primary {
		if pid != "" { // takes precedence over everything else
			cos.Assert(pid == p.si.ID())
		} else if reliable {
			var cnt int // confirmation count
			// double-check
			if cii, cnt = p.bcastHealth(smap); cii.Smap.Version > smap.version() {
				if cii.Smap.Primary.ID != p.si.ID() || cnt < minPidConfirmations {
					glog.Warningf("%s: cannot assume the primary role: local %s < v%d(%s, cnt=%d)",
						p.si, smap, cii.Smap.Version, cii.Smap.Primary.ID, cnt)
					primary = false
					primaryURL = cii.Smap.Primary.PubURL
				} else {
					glog.Warningf("%s: proceeding as primary even though local %s < v%d(%s, cnt=%d)",
						p.si, smap, cii.Smap.Version, cii.Smap.Primary.ID, cnt)
				}
			}
		}
	}

	// 4.1: start as primary
	if primary {
		glog.Infof("%s: assuming primary role for now, starting up...", p.si)
		go p.primaryStartup(smap, config, daemon.cli.ntargets)
		return
	}

	// 4.2: otherwise, join as non-primary
	glog.Infof("%s: starting up as non-primary", p.si)
	err := p.secondaryStartup(smap, primaryURL)
	if err != nil {
		if reliable {
			svm := p.uncoverMeta(smap)
			if svm.Smap != nil && svm.Smap.Primary != nil {
				glog.Infof("%s: second attempt  - joining via %s...", p.si, svm.Smap)
				err = p.secondaryStartup(svm.Smap)
			}
		}
	}
	if err != nil {
		cos.ExitLogf("FATAL: %s (non-primary) failed to join cluster, err: %v", p.si, err)
	}
}

// - make the *primary* decision taking into account both environment and
//   loaded Smap, if exists
// - handle AIS_PRIMARY_ID (TODO: target)
// - see also "change of mind"
func (p *proxyrunner) determineRole(loadedSmap *smapX) (pid string, primary bool) {
	tag := "no Smap, "
	if loadedSmap != nil {
		loadedSmap.Pmap[p.si.ID()] = p.si
		tag = loadedSmap.StringEx() + ", "
	}
	// parse env
	envP := struct {
		pid     string
		primary bool
	}{
		pid:     os.Getenv(cmn.EnvVars.PrimaryID),
		primary: cos.IsParseBool(os.Getenv(cmn.EnvVars.IsPrimary)),
	}

	if envP.pid != "" && envP.primary && p.si.ID() != envP.pid {
		cos.ExitLogf(
			"FATAL: %s: invalid combination of %s=true & %s=%s",
			p.si, cmn.EnvVars.IsPrimary, cmn.EnvVars.PrimaryID, envP.pid,
		)
	}
	glog.Infof("%s: %sprimary-env=%+v", p.si, tag, envP)

	if loadedSmap != nil && envP.pid != "" {
		primary := loadedSmap.GetProxy(envP.pid)
		if primary == nil {
			glog.Errorf(
				"%s: ignoring %s=%s - not found in the loaded %s",
				p.si, cmn.EnvVars.IsPrimary, envP.pid, loadedSmap,
			)
			envP.pid = ""
		} else if loadedSmap.Primary.ID() != envP.pid {
			glog.Warningf(
				"%s: new %s=%s, previous %s",
				p.si, cmn.EnvVars.PrimaryID, envP.pid, loadedSmap.Primary,
			)
			loadedSmap.Primary = primary
		}
	}
	if envP.pid != "" {
		primary = envP.pid == p.si.ID()
		pid = envP.pid
	} else if loadedSmap != nil {
		primary = loadedSmap.isPrimary(p.si)
	} else {
		primary = envP.primary
	}
	return
}

// join cluster
// no change of mind when on the "secondary" track
func (p *proxyrunner) secondaryStartup(smap *smapX, primaryURLs ...string) error {
	if smap == nil {
		smap = newSmap()
	} else if smap.Primary.ID() == p.si.ID() {
		glog.Infof("%s: zeroing-out primary=self in %s", p.si, smap)
		smap.Primary = nil
	}
	p.owner.smap.put(smap)
	if status, err := p.joinCluster(primaryURLs...); err != nil {
		glog.Errorf("%s failed to join cluster (status: %d, err: %v)", p.si, status, err)
		return err
	}

	p.markNodeStarted()

	go func() {
		config := cmn.GCO.Get()
		cii := p.pollClusterStarted(config, smap.Primary)
		if daemon.stopping.Load() {
			return
		}
		if cii != nil {
			if status, err := p.joinCluster(cii.Smap.Primary.CtrlURL, cii.Smap.Primary.PubURL); err != nil {
				glog.Errorf("%s failed to re-join cluster (status: %d, err: %v)", p.si, status, err)
				return
			}
		}
		p.markClusterStarted()
	}()

	glog.Infof("%s: joined as non-primary, %s", p.si, smap.StringEx())
	return nil
}

// Proxy/gateway that is, potentially, the leader of the cluster.
// It waits a configured time for other nodes to join,
// discovers cluster-wide metadata, and resolve remaining conflicts.
func (p *proxyrunner) primaryStartup(loadedSmap *smapX, config *cmn.Config, ntargets int) {
	var (
		smap          = newSmap()
		uuid, created string
		haveJoins     bool
	)

	// 1: init Smap to accept reg-s
	p.owner.smap.Lock()
	si := p.si.Clone()
	smap.Primary = si
	smap.addProxy(si)
	if loadedSmap != nil {
		smap.UUID = loadedSmap.UUID
	}
	p.owner.smap.put(smap)
	p.owner.smap.Unlock()

	p.markNodeStarted()

	if !daemon.cli.skipStartup {
		maxVerSmap := p.acceptRegistrations(smap, loadedSmap, config, ntargets)
		if maxVerSmap != nil {
			if _, err := maxVerSmap.IsDuplicate(p.si); err != nil {
				cos.ExitLogf("FATAL: %v", err)
			}
			maxVerSmap.Pmap[p.si.ID()] = p.si
			p.owner.smap.put(maxVerSmap)
			glog.Infof("%s: change-of-mind #1: registering with %s(%s)",
				p.si, maxVerSmap.Primary.ID(), maxVerSmap.Primary.URL(cmn.NetworkIntraControl))
			if err := p.secondaryStartup(maxVerSmap); err != nil {
				cos.ExitLogf("FATAL: %v", err)
			}
			return
		}
	}

	smap = p.owner.smap.get()
	haveJoins = smap.CountTargets() > 0 || smap.CountProxies() > 1

	// 2: merging local => boot
	if haveJoins {
		p.owner.smap.Lock()
		var (
			added int
			clone = p.owner.smap.get().clone()
		)
		if loadedSmap != nil {
			added, _ = clone.merge(loadedSmap, true /*override (IP, port) duplicates*/)
			clone = loadedSmap
			if added > 0 {
				clone.Version = clone.Version + int64(added) + 1
			}
		}
		bmd := p.owner.bmd.get()
		glog.Infof("%s: initial %s (curr %s, added=%d), initial %s", p.si, loadedSmap, clone.StringEx(), added, bmd.StringEx())
		uuid, created, bmd = p.regpoolMaxVer(clone, bmd)
		clone.UUID, clone.CreationTime = uuid, created
		glog.Infof("%s: regpool %s %s", p.si, clone.StringEx(), bmd.StringEx())
		smap = clone
		p.owner.smap.put(clone)
		p.owner.smap.Unlock()
		msg := p.newAmsgStr(metaction1, bmd)
		wg := p.metasyncer.sync(revsPair{smap, msg}, revsPair{bmd, msg})
		wg.Wait()
	} else {
		glog.Infof("%s: no registrations yet", p.si)
		if loadedSmap != nil {
			glog.Infof("%s: keep going w/ local %s", p.si, loadedSmap.StringEx())
			p.owner.smap.Lock()
			smap = loadedSmap
			p.owner.smap.put(smap)
			p.owner.smap.Unlock()
		}
	}

	// 3: discover cluster meta and resolve remaining conflicts, if any
	p.discoverMeta(smap)

	// 4: still primary?
	p.owner.smap.Lock()
	smap = p.owner.smap.get()
	if !smap.isPrimary(p.si) {
		p.owner.smap.Unlock()
		glog.Infof("%s: registering with primary %s", p.si, smap.Primary)
		if err := p.secondaryStartup(smap); err != nil {
			cos.ExitLogf("FATAL: %v", err)
		}
		return
	}

	// 5:  persist and finalize w/ sync + BMD
	if smap.UUID == "" {
		if !daemon.cli.skipStartup && smap.CountTargets() == 0 {
			cos.ExitLogf("FATAL: %s cannot create a new cluster with no targets, %s", p.si, smap)
		}
		clone := smap.clone()
		if uuid == "" {
			clone.UUID, clone.CreationTime = newClusterUUID()
		} else {
			clone.UUID, clone.CreationTime = uuid, created
		}
		clone.Version++
		p.owner.smap.put(clone)
		smap = clone
	}
	// try to start with a fully staffed IC
	if l := smap.ICCount(); l < smap.DefaultICSize() {
		clone := smap.clone()
		clone.staffIC()
		if l != clone.ICCount() {
			clone.Version++
			smap = clone
			p.owner.smap.put(smap)
		}
	}
	if err := p.owner.smap.persist(smap); err != nil {
		cos.ExitLogf("%s (primary), err: %v", p.si, err)
	}
	p.owner.smap.Unlock()

	// 6. initialize BMD
	bmd := p.owner.bmd.get().clone()
	if bmd.Version == 0 {
		bmd.Version = 1 // init BMD
		bmd.UUID = smap.UUID
		if err := p.owner.bmd.putPersist(bmd, nil); err != nil {
			cos.ExitLogf("%v", err)
		}
	}

	// 6.5. mark RMD as starting up to prevent joining targets from triggering rebalance
	ok := p.owner.rmd.startup.CAS(false, true)
	debug.Assert(ok)

	// 7. global config
	cluConfig, err := p._config(smap.UUID)
	if err != nil {
		cos.ExitLogf("%v", err)
	}

	// 8. metasync (smap, config & bmd) and startup as primary
	var (
		aisMsg = p.newAmsgStr(metaction2, bmd)
		pairs  = []revsPair{{smap, aisMsg}, {bmd, aisMsg}, {cluConfig, aisMsg}}
	)
	wg := p.metasyncer.sync(pairs...)
	wg.Wait()
	p.markClusterStarted()
	glog.Infof("%s: primary & cluster startup complete (%s, %s)", p.si, smap.StringEx(), bmd.StringEx())

	// Clear regpool
	p.reg.mtx.Lock()
	p.reg.pool = nil
	p.reg.mtx.Unlock()

	// 9. resume rebalance if needed
	if config.Rebalance.Enabled {
		p.resumeReb(smap, config)
	}
	p.owner.rmd.startup.Store(false)
}

func (p *proxyrunner) _config(uuid string) (config *globalConfig, err error) {
	var (
		c        cmn.ClusterConfig
		confPath string
	)
	if p.owner.config.version() == 0 {
		config, err = p.initClusterConfig(uuid)
		return
	}
	config, err = p.owner.config.get()
	if !daemon.cli.overrideBackends {
		return
	}
	// load plain-text backends and compare
	confPath = cmn.GCO.GetGlobalConfigPath()
	_, err = jsp.Load(confPath, &c, jsp.Plain())
	if err != nil {
		return
	}
	if c.Backend.EqualClouds(&config.Backend) {
		return
	}
	// NOTE: primary command-line flag `-override_backends` allows to choose
	//       Cloud providers (and HDFS) at deployment time
	config, err = p.owner.config.modify(&configModifier{
		pre: func(ctx *configModifier, clone *globalConfig) (updated bool, err error) {
			clone.Backend.Conf = c.Backend.Conf
			updated = true
			return
		},
	})
	return
}

func (p *proxyrunner) initClusterConfig(uuid string) (config *globalConfig, err error) {
	debug.Assert(p.owner.smap.get().isPrimary(p.si))

	// Create version 1 of the cluster config and set primary URL.
	return p.owner.config.modify(&configModifier{
		pre: func(ctx *configModifier, clone *globalConfig) (updated bool, err error) {
			debug.Assert(clone.version() == 0)
			clone.Proxy.PrimaryURL = p.si.URL(cmn.NetworkPublic)
			clone.UUID = uuid
			updated = true
			return
		},
	})
}

// resume rebalance if needed
func (p *proxyrunner) resumeReb(smap *smapX, config *cmn.Config) {
	var (
		ver     = smap.version()
		nojoins = config.Timeout.MaxHostBusy // initial quiet time with no new joins
		sleep   = cos.CalcProbeFreq(nojoins)
	)
	debug.AssertNoErr(smap.validate())
until:
	// until (last-Smap-update + nojoins)
	for elapsed := time.Duration(0); elapsed < nojoins; {
		time.Sleep(sleep)
		elapsed += sleep
		smap = p.owner.smap.get()
		if !smap.IsPrimary(p.si) {
			debug.AssertNoErr(newErrNotPrimary(p.si, smap))
			return
		}
		if smap.version() != ver {
			debug.Assert(smap.version() > ver)
			elapsed, nojoins = 0, cos.MinDuration(nojoins+sleep, config.Timeout.Startup)
			ver = smap.version()
		}
	}
	debug.AssertNoErr(smap.validate())
	//
	// NOTE: under Smap lock to serialize vs node joins (see httpclupost)
	//
	p.owner.smap.Lock()
	if !p.owner.rmd.rebalance.CAS(true, false) {
		p.owner.smap.Unlock() // nothing to do
		return
	}
	smap = p.owner.smap.get()
	if smap.version() != ver {
		p.owner.smap.Unlock()
		goto until
	}
	if smap.CountActiveTargets() < 2 {
		err := &errNotEnoughTargets{p.si, smap, 2}
		cos.ExitLogf("%s: cannot resume global rebalance - %v", p.si, err)
	}
	var (
		msg    = &cmn.ActionMsg{Action: cmn.ActRebalance, Value: metaction3}
		aisMsg = p.newAmsg(msg, nil)
		ctx    = &rmdModifier{
			pre: func(_ *rmdModifier, clone *rebMD) { clone.Version += 100 },
		}
	)
	rmd, err := p.owner.rmd.modify(ctx)
	if err != nil {
		cos.ExitLogf("%v", err)
	}
	wg := p.metasyncer.sync(revsPair{rmd, aisMsg})
	p.owner.rmd.startup.Store(false)
	p.owner.smap.Unlock()

	wg.Wait()
	glog.Errorf("%s: resuming global rebalance (%s, %s)", p.si, smap.StringEx(), rmd.String())
}

// maxVerSmap != nil iff there's a primary change _and_ the cluster has moved on
func (p *proxyrunner) acceptRegistrations(smap, loadedSmap *smapX, config *cmn.Config,
	ntargets int) (maxVerSmap *smapX) {
	const quiescentIter = 4 // Number of iterations to consider the cluster quiescent.
	var (
		deadlineTime         = config.Timeout.Startup
		checkClusterInterval = deadlineTime / quiescentIter
		sleepDuration        = checkClusterInterval / 5

		definedTargetCnt = ntargets > 0
		doClusterCheck   = loadedSmap != nil && loadedSmap.CountTargets() != 0
	)
	for wait, iter := time.Duration(0), 0; wait < deadlineTime && iter < quiescentIter; wait += sleepDuration {
		time.Sleep(sleepDuration)
		// Check the cluster Smap only once at max.
		if doClusterCheck && wait >= checkClusterInterval {
			if bcastSmap := p.bcastMaxVerBestEffort(loadedSmap); bcastSmap != nil {
				maxVerSmap = bcastSmap
				return
			}
			doClusterCheck = false
		}

		prevTargetCnt := smap.CountTargets()
		smap = p.owner.smap.get()
		if !smap.isPrimary(p.si) {
			break
		}
		targetCnt := smap.CountTargets()
		if targetCnt > prevTargetCnt || (definedTargetCnt && targetCnt < ntargets) {
			// Reset the counter in case there are new targets or we wait for
			// targets but we still don't have enough of them.
			iter = 0
		} else {
			iter++
		}
	}

	targetCnt := p.owner.smap.get().CountTargets()
	if definedTargetCnt {
		if targetCnt >= ntargets {
			glog.Infof("%s: reached expected %d targets (registered: %d)", p.si, ntargets, targetCnt)
		} else {
			glog.Warningf("%s: timed-out waiting for %d targets (registered: %d)",
				p.si, ntargets, targetCnt)
		}
	} else {
		glog.Infof("%s: registered %d new targets", p.si, targetCnt)
	}
	return
}

// the final major step in the primary startup sequence:
// discover cluster-wide metadata and resolve remaining conflicts
func (p *proxyrunner) discoverMeta(smap *smapX) {
	svm := p.uncoverMeta(smap)
	if svm.BMD != nil {
		p.owner.bmd.Lock()
		bmd := p.owner.bmd.get()
		if bmd == nil || bmd.version() < svm.BMD.version() {
			glog.Infof("%s: override local %s with %s", p.si, bmd, svm.BMD)
			if err := p.owner.bmd.putPersist(svm.BMD, nil); err != nil {
				cos.ExitLogf("%v", err)
			}
		}
		p.owner.bmd.Unlock()
	}
	if svm.RMD != nil {
		p.owner.rmd.Lock()
		rmd := p.owner.rmd.get()
		if rmd == nil || rmd.version() < svm.RMD.version() {
			glog.Infof("%s: override local %s with %s", p.si, rmd, svm.RMD)
			p.owner.rmd.put(svm.RMD)
		}
		p.owner.rmd.Unlock()
	}

	if svm.Config != nil {
		p.owner.config.Lock()
		config := cmn.GCO.Get()
		if config.Version < svm.Config.version() {
			if !cos.IsValidUUID(svm.Config.UUID) {
				debug.AssertMsg(false, svm.Config.String())
				cos.ExitLogf("%s: invalid config UUID: %s", p.si, svm.Config)
			}
			if cos.IsValidUUID(config.UUID) && config.UUID != svm.Config.UUID {
				glog.Errorf("Warning: configs have different UUIDs: (%s, %s) vs %s - proceeding anyway",
					p.si, config, svm.Config)
			} else {
				glog.Infof("%s: override local %s with %s", p.si, config, svm.Config)
			}
			p.owner.config.updateGCO(svm.Config)
		}
		p.owner.config.Unlock()
	}

	if svm.Smap == nil || svm.Smap.version() == 0 {
		glog.Infof("%s: no max-ver Smaps", p.si)
		return
	}
	glog.Infof("%s: local %s max-ver %s", p.si, smap.StringEx(), svm.Smap.StringEx())
	smapUUID, sameUUID, sameVersion, eq := smap.Compare(&svm.Smap.Smap)
	if !sameUUID {
		// FATAL: cluster integrity error (cie)
		cos.ExitLogf("%s: split-brain uuid [%s %s] vs %s", ciError(10), p.si, smap.StringEx(), svm.Smap.StringEx())
	}
	if eq && sameVersion {
		return
	}
	if svm.Smap.Primary != nil && svm.Smap.Primary.ID() != p.si.ID() {
		if svm.Smap.version() > smap.version() {
			if dupNode, err := svm.Smap.IsDuplicate(p.si); err != nil {
				if !svm.Smap.IsPrimary(dupNode) {
					cos.ExitLogf("%v", err)
				}
				// If the primary in max-ver Smap version and current node only differ by `DaemonID`,
				// overwrite the proxy entry with current `Snode` and proceed to merging Smap.
				// TODO: Add validation to ensure `dupNode` and `p.si` only differ in `DaemonID`.
				svm.Smap.Primary = p.si
				svm.Smap.delProxy(dupNode.ID())
				svm.Smap.Pmap[p.si.ID()] = p.si
				goto merge
			}
			glog.Infof("%s: change-of-mind #2 %s <= max-ver %s", p.si, smap.StringEx(), svm.Smap.StringEx())
			svm.Smap.Pmap[p.si.ID()] = p.si
			p.owner.smap.put(svm.Smap)
			return
		}
		// FATAL: cluster integrity error (cie)
		cos.ExitLogf("%s: split-brain local [%s %s] vs %s", ciError(20), p.si, smap.StringEx(), svm.Smap.StringEx())
	}
merge:
	p.owner.smap.Lock()
	clone := p.owner.smap.get().clone()
	if !eq {
		glog.Infof("%s: merge local %s <== %s", p.si, clone, svm.Smap)
		_, err := svm.Smap.merge(clone, false /*err if detected (IP, port) duplicates*/)
		if err != nil {
			cos.ExitLogf("%s: %v vs %s", p.si, err, svm.Smap.StringEx())
		}
	} else {
		clone.UUID = smapUUID
	}
	clone.Version = cos.MaxI64(clone.version(), svm.Smap.version()) + 1
	p.owner.smap.put(clone)
	p.owner.smap.Unlock()
	glog.Infof("%s: merged %s", p.si, clone.pp())
}

func (p *proxyrunner) uncoverMeta(bcastSmap *smapX) (svm cluMeta) {
	var (
		err         error
		suuid       string
		config      = cmn.GCO.Get()
		now         = time.Now()
		deadline    = now.Add(config.Timeout.Startup)
		l           = bcastSmap.Count()
		bmds        = make(bmds, l)
		smaps       = make(smaps, l)
		done, slowp bool
	)
	for {
		if daemon.stopping.Load() {
			svm.Smap = nil
			return
		}
		last := time.Now().After(deadline)
		svm, done, slowp = p.bcastMaxVer(bcastSmap, bmds, smaps)
		if done || last {
			break
		}
		time.Sleep(config.Timeout.CplaneOperation)
	}
	if !slowp {
		return
	}
	glog.Infof("%s: slow path...", p.si)
	if svm.BMD, err = resolveUUIDBMD(bmds); err != nil {
		if _, split := err.(*errBmdUUIDSplit); split {
			cos.ExitLogf("%s (primary), err: %v", p.si, err) // cluster integrity error
		}
		glog.Error(err)
	}
	for si, smap := range smaps {
		if !si.IsTarget() {
			continue
		}
		if !cos.IsValidUUID(smap.UUID) {
			continue
		}
		if suuid == "" {
			suuid = smap.UUID
			if suuid != "" {
				glog.Infof("%s: set Smap UUID = %s(%s)", p.si, si, suuid)
			}
		} else if suuid != smap.UUID {
			// FATAL: cluster integrity error (cie)
			cos.ExitLogf("%s: split-brain [%s %s] vs [%s %s]", ciError(30), p.si, suuid, si, smap.UUID)
		}
	}
	for _, smap := range smaps {
		if smap.UUID != suuid {
			continue
		}
		if svm.Smap == nil {
			svm.Smap = smap
		} else if svm.Smap.version() < smap.version() {
			svm.Smap = smap
		}
	}
	return
}

func (p *proxyrunner) bcastMaxVer(bcastSmap *smapX, bmds bmds, smaps smaps) (out cluMeta, done, slowp bool) {
	var (
		borigin, sorigin string
		args             = allocBcastArgs()
	)
	args.req = cmn.ReqArgs{
		Path:  cmn.URLPathDaemon.S,
		Query: url.Values{cmn.URLParamWhat: []string{cmn.GetWhatSmapVote}},
	}
	args.smap = bcastSmap
	args.to = cluster.AllNodes
	args.fv = func() interface{} { return &cluMeta{} }
	results := p.bcastGroup(args)
	freeBcastArgs(args)
	done = true
	for k := range bmds {
		delete(bmds, k)
	}
	for k := range smaps {
		delete(smaps, k)
	}
	for _, res := range results {
		if res.err != nil {
			done = false
			continue
		}
		svm, ok := res.v.(*cluMeta)
		debug.Assert(ok)
		if svm.BMD != nil && svm.BMD.version() > 0 {
			if out.BMD == nil { // 1. init
				borigin, out.BMD = svm.BMD.UUID, svm.BMD
			} else if borigin != "" && borigin != svm.BMD.UUID { // 2. slow path
				slowp = true
			} else if !slowp && out.BMD.Version < svm.BMD.Version { // 3. fast path max(version)
				out.BMD = svm.BMD
				borigin = svm.BMD.UUID
			}
		}
		if svm.RMD != nil && svm.RMD.version() > 0 {
			if out.RMD == nil { // 1. init
				out.RMD = svm.RMD
			} else if !slowp && out.RMD.Version < svm.RMD.Version { // 3. fast path max(version)
				out.RMD = svm.RMD
			}
		}
		if svm.Config != nil && svm.Config.version() > 0 {
			if out.Config == nil { // 1. init
				out.Config = svm.Config
			} else if !slowp && out.Config.version() < svm.Config.version() { // 3. fast path max(version)
				out.Config = svm.Config
			}
		}
		if svm.Smap != nil && svm.VoteInProgress {
			var s string
			if svm.Smap.Primary != nil {
				s = " of the current one " + svm.Smap.Primary.ID()
			}
			glog.Warningf("%s: starting up as primary(?) during reelection%s", p.si, s)
			out.Smap, out.BMD, out.RMD = nil, nil, nil // zero-out as unusable
			done = false
			break
		}
		if svm.Smap != nil && svm.Smap.version() > 0 {
			if out.Smap == nil { // 1. init
				sorigin, out.Smap = svm.Smap.UUID, svm.Smap
			} else if sorigin != "" && sorigin != svm.Smap.UUID { // 2. slow path
				slowp = true
			} else if !slowp && out.Smap.Version < svm.Smap.Version { // 3. fast path max(version)
				out.Smap = svm.Smap
				sorigin = svm.Smap.UUID
			}
		}
		if bmds != nil && svm.BMD != nil && svm.BMD.version() > 0 {
			bmds[res.si] = svm.BMD
		}
		if smaps != nil && svm.Smap != nil && svm.Smap.version() > 0 {
			smaps[res.si] = svm.Smap
		}
	}
	freeCallResults(results)
	return
}

func (p *proxyrunner) bcastMaxVerBestEffort(smap *smapX) *smapX {
	svm, _, slowp := p.bcastMaxVer(smap, nil, nil)
	if svm.Smap != nil && !slowp {
		if svm.Smap.UUID == smap.UUID && svm.Smap.version() > smap.version() && svm.Smap.validate() == nil {
			if svm.Smap.Primary.ID() != p.si.ID() {
				glog.Warningf("%s: primary change whereby local %s is older than max-ver %s",
					p.si, smap.StringEx(), svm.Smap.StringEx())
				return svm.Smap
			}
		}
	}
	return nil
}

func (p *proxyrunner) regpoolMaxVer(smap *smapX, bmd *bucketMD) (uuid, created string, maxverBMD *bucketMD) {
	maxverSmap := smap
	maxverBMD = bmd
	p.reg.mtx.RLock()
	defer p.reg.mtx.RUnlock()
	if len(p.reg.pool) == 0 {
		uuid, created = newClusterUUID()
		return
	}
	for _, regReq := range p.reg.pool {
		if regReq.Smap != nil && regReq.Smap.version() > 0 && cos.IsValidUUID(regReq.Smap.UUID) {
			if maxverSmap != nil && maxverSmap.version() > 0 {
				if cos.IsValidUUID(maxverSmap.UUID) && maxverSmap.UUID != regReq.Smap.UUID {
					cos.ExitLogf("%s: Smap UUIDs don't match: [%s %s]i vs %s", ciError(10),
						p.si, maxverSmap.StringEx(), regReq.Smap.StringEx())
				}
			}
			if maxverSmap == nil || maxverSmap.version() < regReq.Smap.version() {
				maxverSmap = regReq.Smap
			}
		}
		if regReq.BMD != nil && regReq.BMD.version() > 0 && cos.IsValidUUID(regReq.BMD.UUID) {
			if maxverBMD != nil && maxverBMD.version() > 0 {
				if cos.IsValidUUID(maxverBMD.UUID) && maxverBMD.UUID != regReq.BMD.UUID {
					cos.ExitLogf("%s: BMD UUIDs don't match: [%s %s]i vs %s", ciError(10),
						p.si, maxverBMD.StringEx(), regReq.BMD.StringEx())
				}
			}
			if maxverBMD == nil || maxverBMD.version() < regReq.BMD.version() {
				maxverBMD = regReq.BMD
			}
		}
	}
	if maxverSmap == nil || maxverSmap.version() == 0 || !cos.IsValidUUID(maxverSmap.UUID) {
		uuid, created = newClusterUUID()
	} else {
		uuid, created = maxverSmap.UUID, maxverSmap.CreationTime
	}
	if maxverBMD != bmd {
		if err := p.owner.bmd.putPersist(maxverBMD, nil); err != nil {
			cos.ExitLogf("FATAL: %v", err)
		}
		glog.Infof("%s: discovered via regpool and stored %s", p.si, maxverBMD.StringEx())
	}
	return
}
