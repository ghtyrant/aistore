// Package xs is a collection of eXtended actions (xactions), including multi-object
// operations, list-objects, (cluster) rebalance and (target) resilver, ETL, and more.
/*
 * Copyright (c) 2021-2025, NVIDIA CORPORATION. All rights reserved.
 */
package xs

import (
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/atomic"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/cmn/nlog"
	"github.com/NVIDIA/aistore/core"
	"github.com/NVIDIA/aistore/core/meta"
	"github.com/NVIDIA/aistore/ext/etl"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/memsys"
	"github.com/NVIDIA/aistore/stats"
	"github.com/NVIDIA/aistore/transport"
	"github.com/NVIDIA/aistore/xact"
	"github.com/NVIDIA/aistore/xact/xreg"
)

// The flow: `BeginMsg`, `ContMsg`, and `doMsg`.
// Single xaction to execute multiple list-range-prefix API calls for a given pair of (source, destination) buckets.

const PrefixTcoID = "tco-"

type (
	tcoFactory struct {
		args *xreg.TCObjsArgs
		streamingF
	}
	XactTCO struct {
		copier
		transform etl.Session // stateful etl Session
		args      *xreg.TCObjsArgs
		workCh    chan *cmn.TCOMsg
		pending   struct {
			m   map[string]*tcowi
			mtx sync.RWMutex
		}
		streamingX
		chanFull atomic.Int64
		owt      cmn.OWT
	}
	tcowi struct {
		r   *XactTCO
		msg *cmn.TCOMsg
		// finishing
		refc atomic.Int32
	}
)

type (
	// remove objects not present at the source (when synchronizing bckFrom => bckTo)
	syncwi struct {
		rp *prune
	}
)

// interface guard
var (
	_ core.Xact      = (*XactTCO)(nil)
	_ lrxact         = (*XactTCO)(nil)
	_ xreg.Renewable = (*tcoFactory)(nil)
	_ lrwi           = (*tcowi)(nil)
	_ lrwi           = (*syncwi)(nil)
)

////////////////
// tcoFactory //
////////////////

func (p *tcoFactory) New(args xreg.Args, bckFrom *meta.Bck) xreg.Renewable {
	np := &tcoFactory{streamingF: streamingF{RenewBase: xreg.RenewBase{Args: args, Bck: bckFrom}, kind: p.kind}}
	np.args = args.Custom.(*xreg.TCObjsArgs)
	return np
}

func (p *tcoFactory) Start() error {
	//
	// target-local generation of a global UUID
	//
	uuid, err := p.genBEID(p.args.BckFrom, p.args.BckTo)
	if err != nil {
		return err
	}
	p.Args.UUID = PrefixTcoID + uuid

	// new x-tco
	workCh := make(chan *cmn.TCOMsg, maxNumInParallel)
	r := &XactTCO{streamingX: streamingX{p: &p.streamingF, config: cmn.GCO.Get()}, args: p.args, workCh: workCh}
	r.pending.m = make(map[string]*tcowi, maxNumInParallel)
	r.owt = cmn.OwtCopy

	if p.kind == apc.ActETLObjects {
		r.owt = cmn.OwtTransform
		// TODO: when the xctn itself encounters unrecoverable error
		// call r.transform.Finish() to cleanup communicator state
		r.copier.getROC, r.transform, err = etl.GetOfflineTransform(p.args.Msg.Transform.Name, r)
		if err != nil {
			return err
		}
	}

	p.xctn = r
	r.DemandBase.Init(p.UUID(), p.Kind(), "" /*ctlmsg via SetCtlMsg later*/, p.Bck, xact.IdleDefault) // TODO ctlmsg: arch, tco

	smap := core.T.Sowner().Get()
	r.rate.init(p.args.BckFrom, p.args.BckTo, smap.CountActiveTs())

	var sizePDU int32
	if p.kind == apc.ActETLObjects {
		sizePDU = memsys.DefaultBufSize // `transport` to generate PDU-based traffic
	}

	if useDM := !r.args.DisableDM; useDM {
		if err := p.newDM(p.Args.UUID /*trname*/, r.recv, r.config, smap, r.owt, sizePDU); err != nil {
			return err
		}
	}

	r.copier.r = r

	// (rgetstats)
	if bck := r.args.BckFrom; bck.IsRemote() {
		r.bp = core.T.Backend(bck)
		r.vlabs = map[string]string{
			stats.VlabBucket: bck.Cname(""),
			stats.VlabXkind:  r.Kind(),
		}
	}

	xact.GoRunW(r)
	return nil
}

/////////////
// XactTCO //
/////////////

func (r *XactTCO) Name() string {
	return fmt.Sprintf("%s => %s", r.streamingX.Name(), r.args.BckTo)
}

func (r *XactTCO) String() string {
	return r.streamingX.String() + " => " + r.args.BckTo.String()
}

func (r *XactTCO) FromTo() (*meta.Bck, *meta.Bck) { return r.args.BckFrom, r.args.BckTo }

func (r *XactTCO) Snap() (snap *core.Snap) {
	snap = &core.Snap{}
	r.ToSnap(snap)

	snap.IdleX = r.IsIdle()
	f, t := r.FromTo()
	snap.SrcBck, snap.DstBck = f.Clone(), t.Clone()
	return
}

func (r *XactTCO) BeginMsg(msg *cmn.TCOMsg) {
	wi := &tcowi{r: r, msg: msg}
	r.pending.mtx.Lock()

	r.pending.m[msg.TxnUUID] = wi
	r.wiCnt.Inc()

	r.pending.mtx.Unlock()
}

func (r *XactTCO) ContMsg(msg *cmn.TCOMsg) {
	r.IncPending()
	r.workCh <- msg

	if l, c := len(r.workCh), cap(r.workCh); l > c/2 {
		runtime.Gosched() // poor man's throttle
		if l == c {
			cnt := r.chanFull.Inc()
			if (cnt >= 10 && cnt <= 20) || (cnt > 0 && cmn.Rom.FastV(5, cos.SmoduleXs)) {
				nlog.Errorln(cos.ErrWorkChanFull, r.Name(), "cnt", cnt)
			}
		}
	}
}

func (r *XactTCO) doMsg(msg *cmn.TCOMsg) (stop bool) {
	debug.Assert(cos.IsValidUUID(msg.TxnUUID), msg.TxnUUID) // (ref050724: in re: ais/plstcx)

	r.pending.mtx.Lock()
	wi, ok := r.pending.m[msg.TxnUUID]
	r.pending.mtx.Unlock()
	if !ok {
		if r.ErrCnt() > 0 {
			return true // stop
		}
		nlog.Errorf("%s: expecting errors in %s, missing txn %q", core.T.String(), r.String(), msg.TxnUUID) // (unlikely)
		return false
	}

	// this target must be active (ref: ignoreMaintenance)
	smap := core.T.Sowner().Get()
	if err := core.InMaintOrDecomm(smap, core.T.Snode(), r); err != nil {
		r.Abort(err)
		return true // stop
	}
	nat := smap.CountActiveTs()
	wi.refc.Store(int32(nat - 1))

	lrit := &lrit{}
	if err := lrit.init(r, &msg.ListRange, r.Bck(), msg.NumWorkers); err != nil {
		r.AddErr(err)
		return !msg.ContinueOnError // stop?
	}

	// run
	var wg *sync.WaitGroup
	{
		var sb strings.Builder
		sb.Grow(160)
		msg.CopyBckMsg.Str(&sb, r.args.BckFrom.Cname(msg.Prefix), r.args.BckTo.Cname(msg.Prepend))
		sb.WriteByte(' ')
		msg.ListRange.Str(&sb, lrit.lrp == lrpPrefix)
		r.Base.SetCtlMsg(sb.String())
	}
	// run
	if msg.Sync && lrit.lrp != lrpList {
		wg = &sync.WaitGroup{}
		wg.Add(1)
		go func(pt *cos.ParsedTemplate, wg *sync.WaitGroup) {
			r.prune(lrit, smap, pt)
			wg.Done()
		}(lrit.pt.Clone(), wg)
	}
	err := lrit.run(wi, smap, true /*prealloc buf*/)
	if wg != nil {
		wg.Wait()
	}

	lrit.wait()
	if r.IsAborted() {
		return true // stop
	}
	if err != nil {
		r.AddErr(err)
	}
	return false
}

func (r *XactTCO) Run(wg *sync.WaitGroup) {
	nlog.Infoln(r.Name())
	wg.Done()
outer:
	for {
		select {
		case msg := <-r.workCh:
			stop := r.doMsg(msg)
			r.DecPending()
			if stop {
				break outer
			}
			r.sendTerm(msg.TxnUUID, nil, nil)
		case <-r.IdleTimer():
			break outer
		case <-r.ChanAbort():
			break outer
		}
	}
	r.fin(true /*unreg Rx*/) // TODO -- FIXME: use sentinels
	if r.ErrCnt() > 0 {
		// (see "expecting errors" and cleanup)
		r.pending.mtx.Lock()
		clear(r.pending.m)
		r.pending.mtx.Unlock()
	}
}

//
// receive
//

// NOTE: strict(est) error handling: abort on any of the errors below
func (r *XactTCO) recv(hdr *transport.ObjHdr, objReader io.Reader, err error) error {
	if err != nil && !cos.IsEOF(err) {
		goto ex
	}

	r.IncPending()
	err = r._recv(hdr, objReader)
	r.DecPending()
	transport.DrainAndFreeReader(objReader)
ex:
	if err != nil && cmn.Rom.FastV(4, cos.SmoduleXs) {
		nlog.Errorln(err)
	}
	return err
}

func (r *XactTCO) _recv(hdr *transport.ObjHdr, objReader io.Reader) error {
	if hdr.Opcode == opDone {
		r.pending.mtx.Lock()
		wi, ok := r.pending.m[cos.UnsafeS(hdr.Opaque)] // txnUUID
		if !ok {
			r.pending.mtx.Unlock()
			_, err := r.JoinErr()
			return err
		}
		refc := wi.refc.Dec()
		if refc == 0 {
			r.wiCnt.Dec()
		}
		r.pending.mtx.Unlock()
		return nil
	}

	debug.Assert(hdr.Opcode == 0)
	lom := core.AllocLOM(hdr.ObjName)
	err := r._put(hdr, objReader, lom)
	core.FreeLOM(lom)
	return err
}

func (r *XactTCO) _put(hdr *transport.ObjHdr, objReader io.Reader, lom *core.LOM) (err error) {
	if err = lom.InitBck(&hdr.Bck); err != nil {
		return
	}
	lom.CopyAttrs(&hdr.ObjAttrs, true /*skip cksum*/)
	params := core.AllocPutParams()
	{
		params.WorkTag = fs.WorkfilePut
		params.Reader = io.NopCloser(objReader)
		params.Cksum = hdr.ObjAttrs.Cksum
		params.Xact = r
		params.Size = hdr.ObjAttrs.Size
		params.OWT = r.owt
	}
	if lom.AtimeUnix() == 0 {
		// TODO: sender must be setting it, remove this `if` when fixed
		lom.SetAtimeUnix(time.Now().UnixNano())
	}
	params.Atime = lom.Atime()
	err = core.T.PutObject(lom, params)
	core.FreePutParams(params)

	if err != nil {
		r.AddErr(err, 5, cos.SmoduleXs)
	} else if cmn.Rom.FastV(5, cos.SmoduleXs) {
		nlog.Infof("%s: tco-Rx %s, size=%d", r.Base.Name(), lom.Cname(), hdr.ObjAttrs.Size)
	}
	return
}

///////////
// tcowi //
///////////

// under ETL, the returned sizes of transformed objects are unknown (`cos.ContentLengthUnknown`)
// until after the transformation; here we are disregarding the size anyway as the stats
// are done elsewhere

func (wi *tcowi) do(lom *core.LOM, lrit *lrit, buf []byte) {
	r := wi.r
	a := r.copier.prepare(lom, r.args.BckTo, &r.args.Msg.TCBMsg, r.config, buf, r.owt)

	// multiple messages per x-tco (compare w/ x-tcb)
	a.LatestVer, a.Sync = wi.msg.LatestVer, wi.msg.Sync

	err := r.copier.do(a, lom, r.p.dm)
	if cos.IsNotExist(err, 0) && lrit.lrp == lrpList {
		r.AddErr(err, 5, cos.SmoduleXs)
	}
}

//
// remove objects not present at the source (when synchronizing bckFrom => bckTo)
// TODO: probabilistic filtering
//

func (r *XactTCO) prune(pruneit *lrit, smap *meta.Smap, pt *cos.ParsedTemplate) {
	rp := prune{r: r, smap: smap}
	rp.bckFrom, rp.bckTo = r.FromTo()

	// tcb use case
	if pruneit.lrp == lrpPrefix {
		rp.prefix = pruneit.prefix
		rp.init(r.config)
		rp.run()
		rp.wait()
		return
	}

	// same range iterator but different bucket
	var syncit lrit
	debug.Assert(pruneit.lrp == lrpRange)

	err := syncit.init(pruneit.parent, pruneit.msg, rp.bckTo, nwpDflt) // TODO -- FIXME: stopCh
	debug.AssertNoErr(err)
	syncit.pt = pt
	syncwi := &syncwi{&rp} // reusing only prune.do (and not init/run/wait)
	syncit.run(syncwi, smap, false /*prealloc buf*/)
	syncit.wait()
}

func (syncwi *syncwi) do(lom *core.LOM, _ *lrit, _ []byte) {
	syncwi.rp.do(lom, nil)
}
