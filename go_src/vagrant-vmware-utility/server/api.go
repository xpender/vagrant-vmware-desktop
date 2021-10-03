package server

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"sync"

	"github.com/hashicorp/vagrant-vmware-desktop/go_src/vagrant-vmware-utility/driver"

	hclog "github.com/hashicorp/go-hclog"
)

type Api struct {
	listener   net.Listener
	router     *RegexpHandler
	inflight   int
	stopChan   chan bool
	reqTracker sync.WaitGroup
	actionSync sync.Mutex
	Halted     bool
	Address    string
	Port       int
	HaltedChan chan bool
	logger     hclog.Logger
	Driver     driver.Driver
}

func Create(bindAddr string, bindPort int, driver driver.Driver, logger hclog.Logger) (*Api, error) {
	logger = logger.Named("api")
	srv := &Api{
		Address:    bindAddr,
		Driver:     driver,
		Port:       bindPort,
		Halted:     true,
		HaltedChan: make(chan bool),
		stopChan:   make(chan bool),
		inflight:   0,
		logger:     logger,
	}

	router := NewRegexpHandler(srv, logger)
	srv.router = router
	return srv, nil
}

func (a *Api) defineRoutes(r *RegexpHandler) error {
	a.logger.Trace("registering routes")
	routes := map[string]func(http.ResponseWriter, *http.Request){
		// VMware Host Adapter Management
		`/vmnet/vmnet(?P<vnet_slot>\d+)/portforward`:                           r.handleVmnetDeviceForward,
		`/vmnet/vmnet(?P<vnet_slot>\d+)/dhcpreserve/(?P<mac>[^/]+)/(?P<ip>.+)`: r.handleVmnetDhcpReserve,
		`/vmnet/(?P<vnet_name>vmnet\d+)/dhcplease/(?P<mac>.+)`:                 r.handleVmnetDhcpLease,
		`/vmnet/(?P<vnet_name>vmnet\d+)`:                                       r.handleVmnetDevice,
		`/vmnet/verify`:                                                        r.handleVmnetVerify,
		`/vmnet`:                                                               r.handleVmnet,
		// VMware Guest Network Adapter Management
		`/vms/(?P<vm_id>[^/]+)/nic/(?P<adapter_id>.+)`: r.handleVmNicAdapter,
		`/vms/(?P<vm_id>[^/]+)/nic`:                    r.handleVmNic,
		`/vms/(?P<vm_id>[^/]+)/ip`:                     r.handleVmIp,
		// Custom Rest API Paths
		`/portforwards`: r.handlePortForwards,
		`/vmware/paths`: r.handleVmwarePaths,
		`/vmware/info`:  r.handleVmwareInfo,
		`/status`:       r.handleStatus,
		`/version`:      r.handleVersion,
		`/`:             r.handleRoot,
	}

	for path, handler := range routes {
		pattern, err := regexp.Compile(`^` + path + `$`)
		if err != nil {
			a.logger.Error("Failed to compile route path %s - %s", path, err)
			return err
		}
		a.router.HandleFunc(pattern, handler)
	}
	return nil
}

func (a *Api) Start() error {
	a.logger.Debug("start api service requested")
	a.actionSync.Lock()
	defer a.actionSync.Unlock()
	if err := a.defineRoutes(a.router); err != nil {
		return err
	}
	a.logger.Info("api service start", "host", a.Address, "port", a.Port)
	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", a.Address, a.Port))
	if err != nil {
		return err
	}
	a.listener = listener
	a.Halted = false
	go a.consume()
	a.logger.Debug("api ready for message consumption")
	return nil
}

func (a *Api) Stop() error {
	a.logger.Debug("stop api service requested")
	a.actionSync.Lock()
	defer a.actionSync.Unlock()
	if a.Halted {
		return errors.New("Server process is currently halted")
	}
	a.logger.Debug("sending stop notification to consumer")
	a.stopChan <- true
	return nil
}

func (a *Api) consume() {
	defer func() {
		a.Halted = true
		a.logger.Debug("sending halt notification")
		a.HaltedChan <- true
	}()

	go http.Serve(a.listener, http.HandlerFunc(a.RequestHandler))
	select {
	case <-a.stopChan:
		a.logger.Debug("stop notification received - closing")
		a.listener.Close()
		a.logger.Trace("wait for inflight requests to complete")
		a.reqTracker.Wait()
		a.logger.Trace("api consumer halted")
	}
}

func (a *Api) RequestHandler(writ http.ResponseWriter, req *http.Request) {
	a.reqTracker.Add(1)
	a.inflight++
	defer func() {
		a.inflight--
		a.reqTracker.Done()
		a.logger.Debug("completed request", "request-id", fmt.Sprintf("%p", req), "headers", writ.Header())
	}()
	a.logger.Debug("starting request", "request-id", fmt.Sprintf("%p", req))
	a.router.ServeHTTP(writ, req)
}

func (a *Api) Inflight() int {
	return a.inflight
}
