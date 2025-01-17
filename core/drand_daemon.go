package core

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/drand/drand/chain"
	"github.com/drand/drand/metrics"
	"github.com/drand/drand/metrics/pprof"
	"github.com/drand/drand/protobuf/drand"

	"github.com/drand/drand/common"
	dhttp "github.com/drand/drand/http"

	"github.com/drand/drand/key"
	"github.com/drand/drand/log"

	"github.com/drand/drand/net"
)

type DrandDaemon struct {
	initialStores   map[string]*key.Store
	beaconProcesses map[string]*BeaconProcess
	chainHashes     map[string]string

	privGateway *net.PrivateGateway
	pubGateway  *net.PublicGateway
	control     net.ControlListener

	handler *dhttp.DrandHandler

	opts *Config
	log  log.Logger

	// global state lock
	state  sync.Mutex
	exitCh chan bool

	// version indicates the base code variant
	version common.Version
}

// NewDrandDaemon creates a new instance of DrandDaemon
func NewDrandDaemon(c *Config) (*DrandDaemon, error) {
	logger := c.Logger()
	if !c.insecure && (c.certPath == "" || c.keyPath == "") {
		return nil, errors.New("config: need to set WithInsecure if no certificate and private key path given")
	}

	drandDaemon := &DrandDaemon{
		opts:            c,
		log:             logger,
		exitCh:          make(chan bool, 1),
		version:         common.GetAppVersion(),
		initialStores:   make(map[string]*key.Store),
		beaconProcesses: make(map[string]*BeaconProcess),
		chainHashes:     make(map[string]string),
	}

	// Add callback to register a new handler for http server after finishing DKG successfully
	c.dkgCallback = func(share *key.Share, group *key.Group) {
		beaconID := group.ID
		if beaconID == "" {
			beaconID = common.DefaultBeaconID
		}
		drandDaemon.state.Lock()
		bp, isPresent := drandDaemon.beaconProcesses[beaconID]
		drandDaemon.state.Unlock()

		if isPresent {
			drandDaemon.AddNewChainHash(beaconID, bp)
			drandDaemon.AddBeaconHandler(beaconID, bp)
		}
	}

	if err := drandDaemon.init(); err != nil {
		return nil, err
	}

	return drandDaemon, nil
}

func (dd *DrandDaemon) RemoteStatus(ctx context.Context, request *drand.RemoteStatusRequest) (*drand.RemoteStatusResponse, error) {
	beaconID, err := dd.readBeaconID(request.Metadata)
	if err != nil {
		return nil, err
	}

	bp, err := dd.getBeaconProcessByID(beaconID)
	if err != nil {
		return nil, err
	}

	return bp.RemoteStatus(ctx, request)
}

func (dd *DrandDaemon) init() error {
	c := dd.opts

	// Set the private API address to the command-line flag, if given.
	// Otherwise, set it to the address associated with stored private key.
	privAddr := c.PrivateListenAddress("")
	pubAddr := c.PublicListenAddress("")

	if privAddr == "" {
		return fmt.Errorf("private listen address cannot be empty")
	}

	// we set our logger name to its node address
	dd.log = dd.log.Named(privAddr)

	// ctx is used to create the gateway below.
	// Gateway constructors (specifically, the generated gateway stubs that require it)
	// do not actually use it, so we are passing a background context to be safe.
	ctx := context.Background()

	var err error
	dd.log.Infow("", "network", "init", "insecure", c.insecure)

	handler, err := dhttp.New(ctx, c.Version(), dd.log.With("server", "http"))
	if err != nil {
		return err
	}

	if pubAddr != "" {
		if dd.pubGateway, err = net.NewRESTPublicGateway(ctx, pubAddr, c.certPath, c.keyPath, c.certmanager,
			handler.GetHTTPHandler(), c.insecure); err != nil {
			return err
		}
	}

	dd.handler = handler
	dd.privGateway, err = net.NewGRPCPrivateGateway(ctx, privAddr, c.certPath, c.keyPath, c.certmanager, dd, c.insecure, c.grpcOpts...)
	if err != nil {
		return err
	}

	p := c.ControlPort()
	dd.control = net.NewTCPGrpcControlListener(dd, p)
	go dd.control.Start()

	dd.log.Infow("DrandDaemon initialized",
		"private_listen", privAddr,
		"control_port", c.ControlPort(),
		"public_listen", pubAddr,
		"folder", c.ConfigFolderMB())
	dd.privGateway.StartAll()
	if dd.pubGateway != nil {
		dd.pubGateway.StartAll()
	}

	return nil
}

// InstantiateBeaconProcess creates a new BeaconProcess linked to beacon with id 'beaconID'
func (dd *DrandDaemon) InstantiateBeaconProcess(beaconID string, store key.Store) (*BeaconProcess, error) {
	if beaconID == "" {
		beaconID = common.DefaultBeaconID
	}

	// we add the BeaconID to our logger's name. Notice the BeaconID never changes.
	logger := dd.log.Named(beaconID)
	bp, err := NewBeaconProcess(logger, store, dd.opts, dd.privGateway, dd.pubGateway)
	if err != nil {
		return nil, err
	}

	dd.state.Lock()
	dd.beaconProcesses[beaconID] = bp
	dd.state.Unlock()

	if bp.dkgDone {
		metrics.DKGStateChange(metrics.DKGDone, beaconID, false)
	} else {
		metrics.DKGStateChange(metrics.DKGNotStarted, beaconID, false)
	}
	metrics.ReshareStateChange(metrics.ReshareIdle, beaconID, false)
	metrics.IsDrandNode.Set(1)
	metrics.DrandStartTimestamp.SetToCurrentTime()

	return bp, nil
}

func (dd *DrandDaemon) AddNewChainHash(beaconID string, bp *BeaconProcess) {
	dd.state.Lock()
	chainHash := chain.NewChainInfo(bp.group).HashString()
	dd.chainHashes[chainHash] = beaconID
	if common.IsDefaultBeaconID(beaconID) {
		dd.chainHashes[common.DefaultChainHash] = beaconID
	}
	dd.state.Unlock()
}

// RemoveBeaconProcess remove a BeaconProcess linked to beacon with id 'beaconID'
func (dd *DrandDaemon) RemoveBeaconProcess(beaconID string, bp *BeaconProcess) {
	if beaconID == "" {
		beaconID = common.DefaultBeaconID
	}

	chainHash := ""
	if bp.group != nil {
		info := chain.NewChainInfo(bp.group)
		chainHash = info.HashString()
	}

	dd.state.Lock()

	delete(dd.beaconProcesses, beaconID)
	delete(dd.chainHashes, chainHash)
	if common.IsDefaultBeaconID(beaconID) {
		delete(dd.chainHashes, common.DefaultChainHash)
	}

	metrics.DKGStateChange(metrics.DKGShutdown, beaconID, false)
	metrics.ReshareStateChange(metrics.ReshareShutdown, beaconID, false)
	metrics.IsDrandNode.Set(1)
	metrics.DrandStartTimestamp.SetToCurrentTime()

	dd.state.Unlock()
}

// AddBeaconHandler adds a handler linked to beacon with chain hash from http server used to
// expose public services
func (dd *DrandDaemon) AddBeaconHandler(beaconID string, bp *BeaconProcess) {
	info := chain.NewChainInfo(bp.group)

	bh := dd.handler.RegisterNewBeaconHandler(&drandProxy{bp}, info.HashString())
	if common.IsDefaultBeaconID(beaconID) {
		dd.handler.RegisterDefaultBeaconHandler(bh)
	}
}

// RemoveBeaconHandler removes a handler linked to beacon with chain hash from http server used to
// expose public services
func (dd *DrandDaemon) RemoveBeaconHandler(beaconID string, bp *BeaconProcess) {
	if bp.group != nil {
		info := chain.NewChainInfo(bp.group)
		dd.handler.RemoveBeaconHandler(info.HashString())
		if common.IsDefaultBeaconID(beaconID) {
			dd.handler.RemoveBeaconHandler(common.DefaultChainHash)
		}
	}
}

// LoadBeacons checks for existing stores and creates the corresponding BeaconProcess
// accordingly to each stored BeaconID
func (dd *DrandDaemon) LoadBeaconsFromDisk(metricsFlag string) error {
	// Load possible existing stores
	stores, err := key.NewFileStores(dd.opts.ConfigFolderMB())
	if err != nil {
		return err
	}

	for beaconID, fs := range stores {
		bp, err := dd.LoadBeaconFromStore(beaconID, fs)
		if err != nil {
			return err
		}

		// Start metrics server
		if metricsFlag != "" {
			_ = metrics.Start(metricsFlag, pprof.WithProfile(), bp.PeerMetrics)
		}
	}

	return nil
}

func (dd *DrandDaemon) LoadBeaconFromDisk(beaconID string) (*BeaconProcess, error) {
	store := key.NewFileStore(dd.opts.ConfigFolderMB(), beaconID)
	return dd.LoadBeaconFromStore(beaconID, store)
}

func (dd *DrandDaemon) LoadBeaconFromStore(beaconID string, store key.Store) (*BeaconProcess, error) {
	bp, err := dd.InstantiateBeaconProcess(beaconID, store)
	if err != nil {
		dd.log.Error("beacon id", beaconID, "can't instantiate randomness beacon. err:", err)
		return nil, err
	}

	freshRun, err := bp.Load()
	if err != nil {
		return nil, err
	}

	if freshRun {
		dd.log.Infow(fmt.Sprintf("beacon id [%s]: will run as fresh install -> expect to run DKG.", beaconID))
	} else {
		dd.log.Infow(fmt.Sprintf("beacon id [%s]: will start running randomness beacon.", beaconID))

		// Add beacon chain hash as a new valid one
		dd.AddNewChainHash(beaconID, bp)

		// Add beacon handler from chain has for http server
		dd.AddBeaconHandler(beaconID, bp)

		// XXX make it configurable so that new share holder can still start if
		// nobody started.
		// drand.StartBeacon(!c.Bool(pushFlag.Name))
		catchup := true
		bp.StartBeacon(catchup)
	}

	return bp, nil
}
