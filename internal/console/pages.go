package console

import (
	"net/http"
)

func (c *Console) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+Prefix+"status", c.signedIn(c.statusPage))
	c.routerRoutes(mux)
	c.modelRoutes(mux)
	c.drafterRoutes(mux)
	c.loadedRoutes(mux)
	c.hostRoutes(mux)
}

func (c *Console) statusPage(w http.ResponseWriter, r *http.Request) {
	data := pageData(r)
	c.statusData(r.Context(), data)
	c.loadedData(r, data)
	c.render(w, r, http.StatusOK, "status", data)
}
