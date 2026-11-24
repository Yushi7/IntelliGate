package router

import (
	"net/http"
	"strings"
)

type Route struct {
	Prefix  string
	Handler http.Handler
}

type Router struct {
	routes []Route
}

func New(routes ...Route) *Router {
	return &Router{routes: routes}
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	for _, route := range r.routes {
		if req.URL.Path == route.Prefix || strings.HasPrefix(req.URL.Path, route.Prefix+"/") {
			route.Handler.ServeHTTP(w, req)
			return
		}
	}
	http.NotFound(w, req)
}
