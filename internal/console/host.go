package console

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/diffsec/sibyl/internal/hardware"
	"github.com/diffsec/sibyl/internal/hostd"
)

func (c *Console) hostRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+Prefix+"hardware", c.signedIn(c.hardwarePage))
	mux.HandleFunc("POST "+Prefix+"hardware", c.signedIn(c.saveHardware))
	mux.HandleFunc("GET "+Prefix+"host", c.signedIn(c.hostPage))
	mux.HandleFunc("POST "+Prefix+"host/hostname", c.signedIn(c.hostChange(c.setHostname)))
	mux.HandleFunc("POST "+Prefix+"host/network", c.signedIn(c.hostChange(c.setNetwork)))
	mux.HandleFunc("POST "+Prefix+"host/network/confirm", c.signedIn(c.hostChange(c.confirmNetwork)))
	mux.HandleFunc("POST "+Prefix+"host/user", c.signedIn(c.hostChange(c.setUser)))
	mux.HandleFunc("POST "+Prefix+"host/user/delete", c.signedIn(c.hostChange(c.deleteUser)))
	mux.HandleFunc("POST "+Prefix+"host/pool", c.signedIn(c.hostChange(c.createPool)))
	mux.HandleFunc("POST "+Prefix+"host/pool/detach", c.signedIn(c.hostChange(c.detachPool)))
	mux.HandleFunc("POST "+Prefix+"status/restart", c.signedIn(c.restart))
	mux.HandleFunc("POST "+Prefix+"status/openwebui", c.signedIn(c.connectOpenWebUI))
	mux.HandleFunc("POST "+Prefix+"status/update/check", c.signedIn(c.statusChange(func(r *http.Request) (string, hostd.Result, error) {
		res, err := c.client.CheckUpdate(r.Context())
		return "update check", res, err
	})))
	mux.HandleFunc("POST "+Prefix+"status/update/apply", c.signedIn(c.statusChange(func(r *http.Request) (string, hostd.Result, error) {
		res, err := c.client.ApplyUpdate(r.Context(), r.PostFormValue("reboot") == "on")
		return "update", res, err
	})))
	mux.HandleFunc("POST "+Prefix+"status/reboot", c.signedIn(c.statusChange(func(r *http.Request) (string, hostd.Result, error) {
		res, err := c.client.Reboot(r.Context())
		return "reboot", res, err
	})))
	mux.HandleFunc("GET "+Prefix+"setup", c.signedIn(c.setupPage))
}

// Hardware.

func (c *Console) hardwarePage(w http.ResponseWriter, r *http.Request) {
	data := pageData(r)
	hw, err := c.client.Hardware(r.Context())
	if err != nil {
		data["Error"] = err.Error()
	}
	gpus, err := c.client.GPUs(r.Context())
	if err != nil {
		data["Error"] = err.Error()
	}
	st, _ := c.client.Status(r.Context())
	backends := st.Backends
	if len(backends) == 0 {
		backends = hardware.Backends(hardware.VariantAMD)
	}
	type row struct {
		Index                int
		Label                string
		Selected, VulkanOnly bool
	}
	var rows []row
	for _, g := range gpus {
		label := fmt.Sprintf("GPU %d · %s · %s %s · %s", g.Index, g.PCIAddr, g.Vendor, g.Arch, gib(int64(g.VRAMTotal)))
		rows = append(rows, row{g.Index, label, slices.Contains(hw.GPUs, g.Index), g.Arch != "" && !hardware.HIPSupported(g.Arch)})
	}
	data["HW"], data["GPUs"], data["Backends"] = hw, rows, backends
	c.render(w, r, http.StatusOK, "hardware", data)
}

func (c *Console) saveHardware(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	hw := hardware.Config{Backend: hardware.Backend(r.PostFormValue("backend")), Layout: hardware.Layout(r.PostFormValue("layout"))}
	for _, s := range r.PostForm["gpus"] {
		n, err := strconv.Atoi(s)
		if err != nil {
			c.back(w, r, "hardware", "", fmt.Errorf("GPU %q is not a number", s))
			return
		}
		hw.GPUs = append(hw.GPUs, n)
	}
	if len(hw.GPUs) == 0 {
		c.back(w, r, "hardware", "", errors.New("select at least one GPU"))
		return
	}
	res, err := c.client.SetHardware(r.Context(), hw)
	done := "hardware saved"
	if res.BackendChanged {
		done += "; the backend changed, so llama-swap restarts"
	}
	for _, is := range res.Issues {
		done += "; " + is.ID + ": " + is.Reason
	}
	c.back(w, r, "hardware", done, err)
}

// Host.

func (c *Console) hostPage(w http.ResponseWriter, r *http.Request) {
	data := pageData(r)
	st, err := c.client.Host(r.Context())
	if err != nil {
		data["Error"] = err.Error()
	}
	var free []map[string]string
	for _, d := range st.Disks {
		if d.Reason == "" {
			confirm := "erase"
			if d.Serial != "" {
				confirm = d.Serial
			}
			free = append(free, map[string]string{"Path": d.Path, "Label": d.Label(), "Confirm": confirm})
		}
	}
	data["Host"], data["FreeDisks"] = st, free
	c.render(w, r, http.StatusOK, "host", data)
}

// hostChange runs a host action and returns to the Host page. sibyl-hostd
// can take longer than the page waits, and the API then answers that the
// request is queued.
func (c *Console) hostChange(fn func(*http.Request) (string, hostd.Result, error)) http.HandlerFunc {
	return c.hostChangeTo("host", fn)
}

// statusChange is hostChange for the Status page.
func (c *Console) statusChange(fn func(*http.Request) (string, hostd.Result, error)) http.HandlerFunc {
	return c.hostChangeTo("status", fn)
}

func (c *Console) hostChangeTo(page string, fn func(*http.Request) (string, hostd.Result, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		what, res, err := fn(r)
		if err == nil && !res.OK && res.Error != "" {
			err = errors.New(res.Error)
		}
		done := what
		if res.Message != "" {
			done += ": " + res.Message
		}
		c.back(w, r, page, done, err)
	}
}

func (c *Console) setHostname(r *http.Request) (string, hostd.Result, error) {
	name := strings.TrimSpace(r.PostFormValue("hostname"))
	res, err := c.client.SetHostname(r.Context(), name)
	return "set hostname", res, err
}

func (c *Console) setNetwork(r *http.Request) (string, hostd.Result, error) {
	n := hostd.Network{Interface: r.PostFormValue("interface"), Method: r.PostFormValue("method")}
	if n.Method == "static" {
		n.Address, n.Gateway = strings.TrimSpace(r.PostFormValue("address")), strings.TrimSpace(r.PostFormValue("gateway"))
		n.DNS = strings.Fields(r.PostFormValue("dns"))
	}
	if err := n.Validate(); err != nil {
		return "", hostd.Result{}, err
	}
	res, err := c.client.SetNetwork(r.Context(), n)
	return "network changed; it rolls back in 120 seconds unless you keep it from the new address", res, err
}

func (c *Console) confirmNetwork(r *http.Request) (string, hostd.Result, error) {
	res, err := c.client.ConfirmNetwork(r.Context())
	return "network change kept", res, err
}

func (c *Console) setUser(r *http.Request) (string, hostd.Result, error) {
	name, gh := strings.TrimSpace(r.PostFormValue("name")), strings.TrimSpace(r.PostFormValue("github"))
	var keys []string
	for _, l := range strings.Split(r.PostFormValue("keys"), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			keys = append(keys, l)
		}
	}
	if len(keys) == 0 && gh == "" {
		return "", hostd.Result{}, errors.New("give an SSH key or a GitHub user")
	}
	res, err := c.client.SetUser(r.Context(), name, keys, gh)
	return "set operator " + name, res, err
}

func (c *Console) deleteUser(r *http.Request) (string, hostd.Result, error) {
	name := r.PostFormValue("name")
	res, err := c.client.DeleteUser(r.Context(), name)
	return "delete operator " + name, res, err
}

func (c *Console) createPool(r *http.Request) (string, hostd.Result, error) {
	res, err := c.client.CreatePool(r.Context(), r.PostFormValue("disk"), strings.TrimSpace(r.PostFormValue("confirm")))
	return "make a model pool", res, err
}

func (c *Console) detachPool(r *http.Request) (string, hostd.Result, error) {
	name := r.PostFormValue("name")
	res, err := c.client.DetachPool(r.Context(), name)
	return "detach " + name, res, err
}

// Status controls.

func (c *Console) restart(w http.ResponseWriter, r *http.Request) {
	unit := r.PostFormValue("unit")
	c.back(w, r, "status", "restart of "+unit+" queued", c.client.Restart(r.Context(), unit))
}

// connectOpenWebUI gives the System API an Open WebUI admin key: pasted,
// or read with an admin login. The System API uses the password once and
// does not keep it.
func (c *Console) connectOpenWebUI(w http.ResponseWriter, r *http.Request) {
	var err error
	switch r.PostFormValue("mode") {
	case "key":
		_, err = c.client.SetOpenWebUIKey(r.Context(), strings.TrimSpace(r.PostFormValue("key")))
	case "create", "signin":
		_, err = c.client.ConnectOpenWebUI(r.Context(), r.PostFormValue("mode") == "create",
			strings.TrimSpace(r.PostFormValue("name")), strings.TrimSpace(r.PostFormValue("email")), r.PostFormValue("password"))
	default:
		err = errors.New("choose how to connect")
	}
	c.back(w, r, "status", "Open WebUI connected", err)
}

func (c *Console) statusData(ctx context.Context, data map[string]any) {
	st, err := c.client.Status(ctx)
	if err != nil {
		data["Error"] = err.Error()
	} else {
		data["Status"] = st
	}
	if o, err := c.client.OpenWebUI(ctx); err == nil {
		data["OWUI"] = o
	}
	if h, err := c.client.Host(ctx); err == nil && h.Update != nil {
		u := h.Update
		data["Update"] = u
		data["UpdateBusy"] = u.Check.Active == "activating" || u.Download.Active == "activating" || u.DownloadReboot.Active == "activating"
		data["UpdateFailed"] = u.Check.Result != "" && u.Check.Result != "success" ||
			u.Download.Result != "" && u.Download.Result != "success" ||
			u.DownloadReboot.Result != "" && u.DownloadReboot.Result != "success"
	}
}

// Setup.

// setupStep is one step of the setup wizard, the same steps as the TUI's.
type setupStep struct {
	Title, Hint, Link string
	Done              bool
}

func (c *Console) setupPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hw, _ := c.client.Hardware(ctx)
	host, _ := c.client.Host(ctx)
	owui, _ := c.client.OpenWebUI(ctx)
	list, _ := c.client.Models(ctx)
	rt, _ := c.client.Router(ctx)
	operators, clients := 0, 0
	for _, u := range host.Users {
		if u.Managed {
			operators++
		}
	}
	for _, cl := range rt.Clients {
		if !cl.Builtin {
			clients++
		}
	}
	steps := []setupStep{
		{"GPUs, backend and layout", "Choose the GPUs for inference, the backend and the default layout.", Prefix + "hardware", len(hw.GPUs) > 0},
		{"Hostname and network", "Set the hostname, and a static address if you want one.", Prefix + "host", host.Hostname != "" && host.Hostname != "localhost"},
		{"Operator accounts and SSH keys", "Add an operator who can run sibyl over SSH.", Prefix + "host", operators > 0},
		{"Open WebUI administrator", "Create the first Open WebUI admin, or connect an existing one.", Prefix + "status", owui.Configured},
		{"First model", "Search Hugging Face. The fit column shows what suits your GPUs.", Prefix + "downloads", len(list) > 0},
		{"API keys for your programs", "Make a client and key for each program. A key shows once.", Prefix + "router#clients", clients > 0},
	}
	c.render(w, r, http.StatusOK, "setup", map[string]any{"Steps": steps})
}
