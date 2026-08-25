package redfish

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/gorilla/mux"
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

// newRouter builds the serving router: generated routes filtered down to the
// implemented ones, wrapped by session auth, with deterministic miss answers.
func newRouter(bmcUser string, bmcPassword string, resourceManager resourcemanager.ResourceManager) *mux.Router {
	apiService := NewAPIService(bmcUser, bmcPassword, resourceManager)
	filter := routeFilter{server.NewDefaultAPIController(apiService)}
	router := server.NewRouter(session.AuthMiddleware(bmcUser, bmcPassword), filter)

	// Answer misses deterministically: 405 (+Allow, RFC 9110 15.5.6) when the
	// path exists under another method, 404 when it does not exist at all.
	// mux's built-in handling cannot provide this: NewRouter registers routes
	// by ranging a map, and Route.Match clears a recorded ErrMethodMismatch
	// whenever a later-tried route has any succeeding matcher — so the
	// built-in 404/405 choice was random per process. Funnel every miss
	// through a path-only probe over the filtered route table instead.
	miss := missHandler(filter.Routes())
	router.NotFoundHandler = miss
	router.MethodNotAllowedHandler = miss

	return router
}

func NewEmulator(ctx context.Context, port int, bmcUser string, bmcPassword string, resourceManager resourcemanager.ResourceManager) *Emulator {
	return &Emulator{
		ctx:         ctx,
		port:        port,
		bmcUser:     bmcUser,
		bmcPassword: bmcPassword,
		server: &http.Server{
			Addr:    fmt.Sprintf(":%d", port),
			Handler: newRouter(bmcUser, bmcPassword, resourceManager),
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

// missHandler serves requests no registered route matched. The probe router
// holds the same paths without method constraints, so a match means the path
// exists and the miss was a method mismatch (405 with the implemented methods
// in Allow); no match means the path is absent entirely (404).
func missHandler(routes server.Routes) http.Handler {
	probe := mux.NewRouter().StrictSlash(true)
	for _, route := range routes {
		probe.Handle(route.Pattern, http.NotFoundHandler())
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var match mux.RouteMatch
		if !probe.Match(r, &match) {
			http.NotFound(w, r)
			return
		}
		var allow []string
		if pattern, err := match.Route.GetPathTemplate(); err == nil {
			for _, route := range routes {
				if route.Pattern == pattern {
					allow = append(allow, route.Method)
				}
			}
		}
		sort.Strings(allow)
		w.Header().Set("Allow", strings.Join(allow, ", "))
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
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
