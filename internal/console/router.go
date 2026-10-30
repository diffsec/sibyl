package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/diffsec/sibyl/internal/client"
	"github.com/diffsec/sibyl/internal/router"
)

func (c *Console) routerRoutes(mux *http.ServeMux) {
	p := Prefix + "router"
	mux.HandleFunc("GET "+p, c.signedIn(c.routerPage))
	mux.HandleFunc("GET "+p+"/live", c.signedIn(c.routerLive))
	mux.HandleFunc("GET "+p+"/profile", c.signedIn(c.profilePage))
	mux.HandleFunc("GET "+p+"/rule", c.signedIn(c.rulePage))
	for path, h := range map[string]func(*http.Request) (string, error){
		"/client/edit":        c.editClient,
		"/client/delete":      c.deleteClient,
		"/provider":           c.setProvider,
		"/provider/delete":    c.deleteProvider,
		"/profile":            c.saveProfile,
		"/profile/delete":     c.deleteProfile,
		"/rule":               c.saveRule,
		"/rule/move":          c.moveRule,
		"/rule/delete":        c.deleteRule,
		"/calibration":        c.saveCalibration,
		"/calibration/apply":  c.applyProposal,
		"/calibration/delete": c.clearSamples,
		"/classifier":         c.saveClassifier,
	} {
		mux.HandleFunc("POST "+p+path, c.signedIn(c.change(h)))
	}
	// A new client key shows once, so these two answer with the page.
	mux.HandleFunc("POST "+p+"/client", c.signedIn(c.newClient))
	mux.HandleFunc("POST "+p+"/client/key", c.signedIn(c.newKey))
}

// change runs one change and sends the browser back to the Router page
// with the result, so a reload does not repeat the change.
func (c *Console) change(fn func(*http.Request) (string, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		done, err := fn(r)
		q := url.Values{}
		if err != nil {
			q.Set("error", err.Error())
		} else {
			q.Set("done", done)
			c.Log.Info("console change", "user", userOf(r), "change", done)
		}
		http.Redirect(w, r, Prefix+"router?"+q.Encode(), http.StatusSeeOther)
	}
}

// routerData reads everything that the Router page shows.
func (c *Console) routerData(ctx context.Context) (map[string]any, error) {
	rt, err := c.client.Router(ctx)
	if err != nil {
		return nil, err
	}
	st, err := c.client.RouterStats(ctx)
	if err != nil {
		return nil, err
	}
	cal, _ := c.client.RouterCalibration(ctx)
	names := make([]string, 0, len(rt.Profiles.Profiles))
	for n := range rt.Profiles.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	cc := rt.Profiles.Calibration
	rate := router.DefaultSampleRate
	if cc.SampleRate != nil {
		rate = *cc.SampleRate
	}
	regen := 0
	for _, n := range st.Regenerated {
		regen += n
	}
	return map[string]any{
		"Router": rt, "Stats": st, "Cal": cal, "ProfileNames": names,
		"Rate": strconv.FormatFloat(rate*100, 'f', -1, 64), "Daily": strconv.FormatFloat(cc.Daily(), 'f', -1, 64),
		"Routes": routeOptions(rt), "Regenerated": regen,
	}, nil
}

func (c *Console) routerPage(w http.ResponseWriter, r *http.Request) {
	data, err := c.routerData(r.Context())
	if err != nil {
		data = map[string]any{"Error": err.Error()}
	}
	data["FlashError"] = r.URL.Query().Get("error")
	c.render(w, r, http.StatusOK, "router", data)
}

// routerLive is the part of the Router page that htmx refreshes.
func (c *Console) routerLive(w http.ResponseWriter, r *http.Request) {
	data, err := c.routerData(r.Context())
	if err != nil {
		data = map[string]any{"Error": err.Error()}
	}
	c.renderBlock(w, r, http.StatusOK, "router", "live", data)
}

// routeOptions lists local, the listed provider models and the routes that
// the profiles use.
func routeOptions(rt client.Router) []string {
	out := []string{"local"}
	add := func(s string) {
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	for _, p := range rt.Providers {
		for _, m := range p.Models {
			add(p.Name + "/" + m)
		}
	}
	for _, p := range rt.Profiles.Profiles {
		for _, r := range p.Rules {
			add(r.Route)
		}
		for _, f := range p.Fallback {
			add(f)
		}
	}
	return out
}

func formFloat(r *http.Request, name string) (float64, error) {
	s := strings.TrimSpace(r.PostFormValue(name))
	if s == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("%s: give a number of 0 or more", name)
	}
	return f, nil
}

func formInt(r *http.Request, name string) (int, error) {
	s := strings.TrimSpace(r.PostFormValue(name))
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s: give a whole number of 0 or more", name)
	}
	return n, nil
}

func formBudget(r *http.Request) (*client.RouterBudget, error) {
	daily, err := formFloat(r, "daily_usd")
	if err != nil {
		return nil, err
	}
	monthly, err := formFloat(r, "monthly_usd")
	if err != nil {
		return nil, err
	}
	if daily == 0 && monthly == 0 {
		return nil, nil
	}
	return &client.RouterBudget{DailyUSD: daily, MonthlyUSD: monthly}, nil
}

func checked(r *http.Request, name string) bool { return r.PostFormValue(name) == "on" }

// The clients.

func (c *Console) newClient(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	b, err := formBudget(r)
	var nc client.NewRouterClient
	if err == nil {
		nc, err = c.client.CreateRouterClient(r.Context(), name, r.PostFormValue("policy"), b)
	}
	c.keyPage(w, r, nc, err, "create client "+name)
}

func (c *Console) newKey(w http.ResponseWriter, r *http.Request) {
	name := r.PostFormValue("name")
	nc, err := c.client.NewRouterClientKey(r.Context(), name)
	c.keyPage(w, r, nc, err, "new key for "+name)
}

// keyPage shows the Router page with a new key. The key is in this
// response only.
func (c *Console) keyPage(w http.ResponseWriter, r *http.Request, nc client.NewRouterClient, err error, what string) {
	data, derr := c.routerData(r.Context())
	if derr != nil {
		data = map[string]any{"Error": derr.Error()}
	}
	if err != nil {
		data["FlashError"] = err.Error()
	} else {
		data["NewKey"], data["NewKeyFor"] = nc.Key, nc.Name
		data["Flash"] = what + ": done"
		c.Log.Info("console change", "user", userOf(r), "change", what)
	}
	c.render(w, r, http.StatusOK, "router", data)
}

func (c *Console) editClient(r *http.Request) (string, error) {
	name := r.PostFormValue("name")
	b, err := formBudget(r)
	if err != nil {
		return "", err
	}
	err = c.client.SetRouterClient(r.Context(), name, r.PostFormValue("policy"), checked(r, "allow_hint"), b, checked(r, "calibrate"))
	return "change client " + name, err
}

func (c *Console) deleteClient(r *http.Request) (string, error) {
	name := r.PostFormValue("name")
	return "delete client " + name, c.client.DeleteRouterClient(r.Context(), name)
}

// The providers.

func (c *Console) setProvider(r *http.Request) (string, error) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	p := client.ProviderRequest{Type: r.PostFormValue("type"), BaseURL: strings.TrimSpace(r.PostFormValue("base_url")),
		APIKey: strings.TrimSpace(r.PostFormValue("api_key")), Models: strings.Fields(r.PostFormValue("models"))}
	rt, err := c.client.Router(r.Context())
	if err != nil {
		return "", err
	}
	// A change keeps the prices and the models.dev ID.
	for _, old := range rt.Providers {
		if old.Name == name {
			p.Prices, p.Catalog = old.Prices, old.Catalog
		}
	}
	return "set provider " + name, c.client.SetRouterProvider(r.Context(), name, p)
}

func (c *Console) deleteProvider(r *http.Request) (string, error) {
	name := r.PostFormValue("name")
	return "delete provider " + name, c.client.DeleteRouterProvider(r.Context(), name)
}

// The profiles and rules. Each change reads the whole profiles file, changes
// one part and writes it back, so the settings that a form does not show
// are kept.

func (c *Console) profiles(ctx context.Context, fn func(*router.ProfilesFile) (string, error)) (string, error) {
	rt, err := c.client.Router(ctx)
	if err != nil {
		return "", err
	}
	var f router.ProfilesFile
	b, err := json.Marshal(rt.Profiles)
	if err == nil {
		err = json.Unmarshal(b, &f)
	}
	if err != nil {
		return "", err
	}
	if f.Profiles == nil {
		f.Profiles = map[string]router.Profile{}
	}
	what, err := fn(&f)
	if err != nil {
		return "", err
	}
	return what, c.client.SetRouterProfiles(ctx, f)
}

func (c *Console) profilePage(w http.ResponseWriter, r *http.Request) {
	rt, err := c.client.Router(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	models, err := c.client.Models(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	name := r.URL.Query().Get("name")
	p, exists := rt.Profiles.Profiles[name]
	var chat []string
	for _, m := range models {
		if !m.Settings.Embedding {
			chat = append(chat, m.ID)
		}
	}
	if p.LocalModel != "" && !slices.Contains(chat, p.LocalModel) {
		chat = append(chat, p.LocalModel)
	}
	c.render(w, r, http.StatusOK, "profile", map[string]any{
		"Name": name, "Exists": exists, "Profile": p, "Models": chat, "Fallback": strings.Join(p.Fallback, " "),
	})
}

func (c *Console) saveProfile(r *http.Request) (string, error) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	wait, err := formInt(r, "local_wait_seconds")
	if err != nil {
		return "", err
	}
	return c.profiles(r.Context(), func(f *router.ProfilesFile) (string, error) {
		p, exists := f.Profiles[name]
		if r.PostFormValue("new") == "1" && exists {
			return "", fmt.Errorf("profile %q exists", name)
		}
		p.LocalModel, p.LocalWaitSeconds = r.PostFormValue("local_model"), wait
		p.Fallback = strings.Fields(r.PostFormValue("fallback"))
		f.Profiles[name] = p
		return "save profile " + name, nil
	})
}

func (c *Console) deleteProfile(r *http.Request) (string, error) {
	name := r.PostFormValue("name")
	return c.profiles(r.Context(), func(f *router.ProfilesFile) (string, error) {
		if _, ok := f.Profiles[name]; !ok {
			return "", fmt.Errorf("no profile %q", name)
		}
		delete(f.Profiles, name)
		return "delete profile " + name, nil
	})
}

// ruleIndex reads the profile and the rule index of a form or query. The
// index is -1 for a new rule.
func ruleIndex(f *router.ProfilesFile, profile, index string) (router.Profile, int, error) {
	p, ok := f.Profiles[profile]
	if !ok {
		return p, 0, fmt.Errorf("no profile %q", profile)
	}
	if index == "" || index == "new" {
		return p, -1, nil
	}
	i, err := strconv.Atoi(index)
	if err != nil || i < 0 || i >= len(p.Rules) {
		return p, 0, errors.New("the rules changed: open the page again")
	}
	return p, i, nil
}

func tri(b *bool) string {
	if b == nil {
		return "any"
	}
	return strconv.FormatBool(*b)
}

func fromTri(s string) *bool {
	if s != "true" && s != "false" {
		return nil
	}
	b := s == "true"
	return &b
}

func (c *Console) rulePage(w http.ResponseWriter, r *http.Request) {
	rt, err := c.client.Router(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	q := r.URL.Query()
	p, i, err := ruleIndex(&rt.Profiles, q.Get("profile"), q.Get("i"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	rule := router.Rule{Route: "local"}
	if i >= 0 {
		rule = p.Rules[i]
	}
	prompt := ""
	if rule.When.PromptTokensOver != nil {
		prompt = rule.When.PromptTokensOver.String()
	}
	var clients []string
	for _, cl := range rt.Clients {
		clients = append(clients, cl.Name)
	}
	routes := routeOptions(rt)
	if !slices.Contains(routes, rule.Route) {
		routes = append(routes, rule.Route)
	}
	tasks, diffs := slices.DeleteFunc(slices.Clone(router.Tasks), func(s string) bool { return s == "unknown" }),
		slices.DeleteFunc(slices.Clone(router.Difficulties), func(s string) bool { return s == "unknown" })
	c.render(w, r, http.StatusOK, "rule", map[string]any{
		"Profile": q.Get("profile"), "Index": i, "Rule": rule, "Routes": routes, "Prompt": prompt,
		"Tasks": tasks, "Difficulties": diffs, "Clients": clients,
		"Sensitive": tri(rule.When.Sensitive), "Needs": tri(rule.When.NeedsProvider),
		"Images": tri(rule.When.HasImages), "Tools": tri(rule.When.HasTools),
		"Tri": []string{"any", "true", "false"},
	})
}

func (c *Console) saveRule(r *http.Request) (string, error) {
	return c.profiles(r.Context(), func(f *router.ProfilesFile) (string, error) {
		prof := r.PostFormValue("profile")
		p, i, err := ruleIndex(f, prof, r.PostFormValue("i"))
		if err != nil {
			return "", err
		}
		var w router.When
		if i >= 0 {
			// The conditions that the form does not show are kept.
			w = p.Rules[i].When
		}
		w.Sensitive, w.NeedsProvider = fromTri(r.PostFormValue("sensitive")), fromTri(r.PostFormValue("needs_provider"))
		w.HasImages, w.HasTools = fromTri(r.PostFormValue("has_images")), fromTri(r.PostFormValue("has_tools"))
		w.Task, w.Difficulty, w.Clients = r.PostForm["task"], r.PostForm["difficulty"], r.PostForm["clients"]
		w.PromptTokensOver = nil
		if s := strings.TrimSpace(r.PostFormValue("prompt_tokens_over")); s != "" {
			var t router.Tokens
			if err := t.UnmarshalJSON([]byte(strconv.Quote(s))); err != nil {
				return "", err
			}
			w.PromptTokensOver = &t
		}
		rule := router.Rule{Name: strings.TrimSpace(r.PostFormValue("rule_name")), When: w, Route: r.PostFormValue("route")}
		if rule.Name == "" {
			return "", errors.New("a rule needs a name")
		}
		if i >= 0 {
			p.Rules[i] = rule
		} else {
			p.Rules = append(p.Rules, rule)
		}
		f.Profiles[prof] = p
		return "save rule " + rule.Name, nil
	})
}

func (c *Console) moveRule(r *http.Request) (string, error) {
	return c.profiles(r.Context(), func(f *router.ProfilesFile) (string, error) {
		prof := r.PostFormValue("profile")
		p, i, err := ruleIndex(f, prof, r.PostFormValue("i"))
		if err != nil || i < 0 {
			return "", errors.New("no such rule")
		}
		j := i - 1
		if r.PostFormValue("dir") == "down" {
			j = i + 1
		}
		if j < 0 || j >= len(p.Rules) {
			return "", errors.New("the rule cannot move further")
		}
		p.Rules[i], p.Rules[j] = p.Rules[j], p.Rules[i]
		f.Profiles[prof] = p
		return "move rule " + p.Rules[j].Name, nil
	})
}

func (c *Console) deleteRule(r *http.Request) (string, error) {
	return c.profiles(r.Context(), func(f *router.ProfilesFile) (string, error) {
		prof := r.PostFormValue("profile")
		p, i, err := ruleIndex(f, prof, r.PostFormValue("i"))
		if err != nil || i < 0 {
			return "", errors.New("no such rule")
		}
		name := p.Rules[i].Name
		p.Rules = slices.Delete(p.Rules, i, i+1)
		f.Profiles[prof] = p
		return "delete rule " + name, nil
	})
}

// Calibration and the classifier.

func (c *Console) saveCalibration(r *http.Request) (string, error) {
	rate, err := formFloat(r, "sample_rate")
	if err != nil || rate > 100 {
		return "", errors.New("sample rate: give a percentage from 0 to 100")
	}
	daily, err := formFloat(r, "daily_usd")
	if err != nil {
		return "", err
	}
	keep, err := formInt(r, "keep_days")
	if err != nil {
		return "", err
	}
	most, err := formInt(r, "max_samples")
	if err != nil {
		return "", err
	}
	return c.profiles(r.Context(), func(f *router.ProfilesFile) (string, error) {
		cc := &f.Calibration
		rate /= 100
		cc.SampleRate, cc.DailyUSD = &rate, &daily
		cc.Judge, cc.Compare = r.PostFormValue("judge"), r.PostFormValue("compare")
		cc.KeepDays, cc.MaxSamples = keep, most
		cc.NoSeed, cc.AutoApply = !checked(r, "seed"), checked(r, "auto_apply")
		return "save the calibration settings", nil
	})
}

func (c *Console) applyProposal(r *http.Request) (string, error) {
	return "apply calibration proposal", c.client.ApplyCalibration(r.Context(), r.PostFormValue("id"))
}

func (c *Console) clearSamples(r *http.Request) (string, error) {
	n, err := c.client.ClearCalibration(r.Context())
	return fmt.Sprintf("delete %d calibration samples", n), err
}

func (c *Console) saveClassifier(r *http.Request) (string, error) {
	k, err := formInt(r, "k")
	if err != nil {
		return "", err
	}
	timeout, err := formInt(r, "timeout_ms")
	if err != nil {
		return "", err
	}
	return c.profiles(r.Context(), func(f *router.ProfilesFile) (string, error) {
		var words []string
		for _, w := range strings.Split(r.PostFormValue("sensitive_words"), ",") {
			if w = strings.TrimSpace(w); w != "" {
				words = append(words, w)
			}
		}
		f.SensitiveWords = words
		cl := &f.Classifier
		cl.Model, cl.K, cl.TimeoutMS, cl.NoStarter = strings.TrimSpace(r.PostFormValue("model")), k, timeout, !checked(r, "starter")
		return "save the classifier settings", nil
	})
}
