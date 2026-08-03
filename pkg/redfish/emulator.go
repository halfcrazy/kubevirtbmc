package redfish

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"
	"kubevirt.io/kubevirtbmc/pkg/generated/redfish/server"
	"kubevirt.io/kubevirtbmc/pkg/resourcemanager"
	"kubevirt.io/kubevirtbmc/pkg/session"
)

type Emulator struct {
	ctx  context.Context
	port int

	bmcUser     string
	bmcPassword string

	wg     sync.WaitGroup
	server *http.Server
}

func NewEmulator(ctx context.Context, port int, bmcUser string, bmcPassword string, resourceManager resourcemanager.ResourceManager) *Emulator {
	apiService := NewAPIService(bmcUser, bmcPassword, resourceManager)
	apiController := server.NewDefaultAPIController(apiService)
	router := server.NewRouter(session.AuthMiddleware(bmcUser, bmcPassword), routeFilter{apiController})

	return &Emulator{
		ctx:         ctx,
		port:        port,
		bmcUser:     bmcUser,
		bmcPassword: bmcPassword,
		server: &http.Server{
			Addr:    fmt.Sprintf(":%d", port),
			Handler: router,
		},
	}
}

func (e *Emulator) Run() error {
	e.wg.Add(1)

	go func() {
		defer e.wg.Done()

		if err := e.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Println(err)
		}
	}()

	return nil
}

func (e *Emulator) Stop() {
	if err := e.server.Shutdown(e.ctx); err != nil {
		fmt.Println(err)
	}
	e.wg.Wait()
	logrus.Info("Redfish emulator gracefully stopped")
}

//go:generate go run ./strip-redfish-routes -input api_service.go -output implemented_routes_gen.go

// routeFilter registers only the routes backed by a real implementation. The
// generated route table holds one entry per Redfish operation (~4000, all but
// a handful answering 501), and gorilla/mux compiles every registered pattern
// into a regexp that stays live for the process lifetime — ~50MB of heap per
// agent pod, see kubevirtbmc/kubevirtbmc#264.
type routeFilter struct {
	inner server.Router
}

func (f routeFilter) Routes() server.Routes {
	routes := f.inner.Routes()
	for name := range routes {
		if !implementedMethods[baseRouteName(name)] {
			delete(routes, name)
		}
	}
	return routes
}

// baseRouteName strips the _N suffix the OpenAPI generator appends to alias
// paths of the same operation (e.g. RedfishV1Get_0 -> RedfishV1Get), so alias
// routes inherit the implementation status of their canonical route.
func baseRouteName(name string) string {
	i := strings.LastIndexByte(name, '_')
	if i < 0 || i+1 == len(name) {
		return name
	}
	for _, c := range name[i+1:] {
		if c < '0' || c > '9' {
			return name
		}
	}
	return name[:i]
}
