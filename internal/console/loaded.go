package console

import (
	"net/http"

	"github.com/diffsec/sibyl/internal/client"
)

func (c *Console) loadedRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+Prefix+"status/loaded", c.signedIn(c.loadedLive))
	mux.HandleFunc("POST "+Prefix+"models/load", c.signedIn(c.loadModel))
	mux.HandleFunc("POST "+Prefix+"models/unload", c.signedIn(c.unloadModel))
}

// loadedData adds what llama-swap runs to data. Running is missing when
// llama-swap does not answer.
func (c *Console) loadedData(r *http.Request, data map[string]any) client.Running {
	run, err := c.client.Running(r.Context())
	if err != nil {
		data["RunningError"] = err.Error()
		return run
	}
	data["Running"] = run
	return run
}

func (c *Console) loadedLive(w http.ResponseWriter, r *http.Request) {
	data := pageData(r)
	c.loadedData(r, data)
	c.renderBlock(w, r, http.StatusOK, "status", "loaded", data)
}

// backPage is the page that a load or unload returns to: the Status page
// or the Models page.
func backPage(r *http.Request) string {
	if r.PostFormValue("back") == "status" {
		return "status"
	}
	return "models"
}

func (c *Console) loadModel(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	err := c.client.LoadModel(r.Context(), id)
	c.back(w, r, backPage(r), "loading "+id+"; llama-swap stops the models that cannot run with it", err)
}

func (c *Console) unloadModel(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	err := c.client.UnloadModel(r.Context(), id)
	c.back(w, r, backPage(r), "unloaded "+id, err)
}
