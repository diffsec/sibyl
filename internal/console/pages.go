package console

import (
	"net/http"
)

func (c *Console) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+Prefix+"status", c.signedIn(c.statusPage))
	c.routerRoutes(mux)
}

func (c *Console) statusPage(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{}
	st, err := c.client.Status(r.Context())
	if err != nil {
		data["Error"] = err.Error()
	} else {
		data["Status"] = st
	}
	c.render(w, r, http.StatusOK, "status", data)
}
