package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/diffsec/sibyl/internal/client"
	"github.com/diffsec/sibyl/internal/hostd"
)

type hostLoaded struct {
	st  hostd.State
	err error
}

type hostDone struct {
	what string
	res  hostd.Result
	err  error
}

type hostTick struct{}

// hostView is the host part of the setup wizard: hostname, network and
// operator accounts. The System API passes each change to sibyl-hostd.
type hostView struct {
	c      *client.Client
	st     hostd.State
	err    error
	cursor int // selected account
	ticks  bool

	form    *huh.Form
	mode    string // hostname, network, user, delete
	name    string
	iface   string
	method  string
	address string
	gateway string
	dns     string
	keys    string
	github  string
	confirm bool
	disk    string
	serial  string
	// secret is a recovery key from the last pool, shown until the next action.
	secret string
}

func newHostView(c *client.Client) *hostView { return &hostView{c: c} }

func (v *hostView) fetch() tea.Cmd {
	return call(func(ctx context.Context) (hostd.State, error) { return v.c.Host(ctx) },
		func(st hostd.State, err error) tea.Msg { return hostLoaded{st, err} })
}

func (v *hostView) activate() tea.Cmd {
	if v.ticks {
		return v.fetch()
	}
	v.ticks = true
	return tea.Batch(v.fetch(), tea.Tick(5*time.Second, func(time.Time) tea.Msg { return hostTick{} }))
}

func (v *hostView) capturing() bool { return v.form != nil }

func (v *hostView) help() string {
	if v.form != nil {
		return "enter next · shift+tab back · esc discard"
	}
	h := "h hostname · n network · a add operator · p new pool · x detach pool"
	if u, ok := v.selected(); ok && u.Managed {
		h += " · ↑/↓ select · k keys · d delete"
	} else {
		h += " · ↑/↓ select"
	}
	if v.st.Pending != nil {
		h = "c keep the network change · " + h
	}
	return h
}

func (v *hostView) selected() (hostd.Account, bool) {
	if v.cursor < 0 || v.cursor >= len(v.st.Users) {
		return hostd.Account{}, false
	}
	return v.st.Users[v.cursor], true
}

func (v *hostView) hostnameForm() tea.Cmd {
	v.mode, v.name = "hostname", v.st.Hostname
	v.form = huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("Hostname").
			Description("Lowercase letters, digits and hyphens. The TLS certificate keeps its names.").
			Value(&v.name).Validate(func(s string) error { return hostd.Hostname{Hostname: s}.Validate() }),
	)).WithShowHelp(false)
	return v.form.Init()
}

func (v *hostView) networkForm() tea.Cmd {
	var opts []huh.Option[string]
	for _, it := range v.st.Interfaces {
		if it.Type == "ethernet" {
			opts = append(opts, huh.NewOption(fmt.Sprintf("%s  %s  %s", it.Name, it.State, strings.Join(it.Addresses, " ")), it.Name))
		}
	}
	if len(opts) == 0 {
		return note(false, "no ethernet interface found")
	}
	v.mode, v.iface, v.method = "network", opts[0].Value, "dhcp"
	v.address, v.gateway, v.dns = "", "", ""
	if it := v.ifaceState(v.iface); it != nil {
		if it.Method == "static" {
			v.method = "static"
		}
		if len(it.Addresses) > 0 {
			v.address = it.Addresses[0]
		}
		v.gateway, v.dns = it.Gateway, strings.Join(it.DNS, " ")
	}
	v.form = huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().Title("Interface").Options(opts...).Value(&v.iface),
			huh.NewSelect[string]().Title("IPv4").Inline(true).
				Options(huh.NewOption("DHCP", "dhcp"), huh.NewOption("Static", "static")).Value(&v.method),
		),
		huh.NewGroup(
			huh.NewInput().Title("Address").Description("With the prefix length, such as 192.168.1.26/24.").Value(&v.address),
			huh.NewInput().Title("Gateway").Description("Leave empty for no default route.").Value(&v.gateway),
			huh.NewInput().Title("DNS servers").Description("Up to 3, separated by spaces.").Value(&v.dns),
		).WithHideFunc(func() bool { return v.method != "static" }),
		huh.NewGroup(
			huh.NewNote().Title("Rollback").Description(
				"The change rolls back after 120 seconds unless you press c on this screen.\n"+
					"If you are connected over the network, reconnect at the new address and keep it from there."),
		),
	).WithShowHelp(false)
	return v.form.Init()
}

func (v *hostView) ifaceState(name string) *hostd.Interface {
	for i := range v.st.Interfaces {
		if v.st.Interfaces[i].Name == name {
			return &v.st.Interfaces[i]
		}
	}
	return nil
}

func (v *hostView) userForm(name string) tea.Cmd {
	v.mode, v.name, v.keys, v.github = "user", name, "", ""
	fields := []huh.Field{}
	if name == "" {
		fields = append(fields, huh.NewInput().Title("User name").
			Description("The account is in sibyl-operators: it can run sibyl, but not sudo.").
			Value(&v.name).Validate(hostd.ValidateUserName))
	} else {
		fields = append(fields, huh.NewNote().Title("Replace the SSH keys of "+name))
	}
	fields = append(fields,
		huh.NewText().Title("SSH public keys").Description("One key per line, such as the content of ~/.ssh/id_ed25519.pub.").
			Lines(4).Value(&v.keys),
		huh.NewInput().Title("GitHub user").Description("Optional. Adds the keys that github.com/<user>.keys lists.").
			Value(&v.github),
	)
	v.form = huh.NewForm(huh.NewGroup(fields...)).WithShowHelp(false)
	return v.form.Init()
}

func (v *hostView) deleteForm(name string) tea.Cmd {
	v.mode, v.name, v.confirm = "delete", name, false
	v.form = huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title("Delete operator " + name + "?").
			Description("This deletes the account and its home directory.").
			Affirmative("Delete").Negative("Keep").Value(&v.confirm),
	)).WithShowHelp(false)
	return v.form.Init()
}

func (v *hostView) poolForm() tea.Cmd {
	var opts []huh.Option[string]
	for _, d := range v.st.Disks {
		if d.Reason == "" {
			opts = append(opts, huh.NewOption(d.Label(), d.Path))
		}
	}
	if len(opts) == 0 {
		return note(false, "no free disk: a pool needs a whole disk of 16 GiB or more that nothing uses")
	}
	v.mode, v.disk, v.serial = "pool", opts[0].Value, ""
	v.form = huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().Title("Disk for the new model pool").
				Description("The disk is erased and encrypted. It unlocks with a key on the system disk.").
				Options(opts...).Value(&v.disk),
		),
		huh.NewGroup(
			huh.NewInput().Title("Confirm").DescriptionFunc(func() string {
				for _, d := range v.st.Disks {
					if d.Path == v.disk && d.Serial != "" {
						return "Everything on " + d.Path + " is deleted. Type its serial number: " + d.Serial
					}
				}
				return "Everything on " + v.disk + " is deleted. Type erase."
			}, &v.disk).Value(&v.serial),
		),
	).WithShowHelp(false)
	return v.form.Init()
}

func (v *hostView) detachForm() tea.Cmd {
	var opts []huh.Option[string]
	for _, p := range v.st.Pools {
		opts = append(opts, huh.NewOption(p.Name+"  "+p.Disk, p.Name))
	}
	if len(opts) == 0 {
		return note(false, "there are no pools")
	}
	v.mode, v.name, v.confirm = "detach", opts[0].Value, false
	v.form = huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().Title("Pool to detach").Options(opts...).Value(&v.name),
		huh.NewConfirm().Title("Detach it?").
			Description("llama-swap unloads its models first. The data and the key file stay on the host.").
			Affirmative("Detach").Negative("Keep").Value(&v.confirm),
	)).WithShowHelp(false)
	return v.form.Init()
}

func (v *hostView) submit() tea.Cmd {
	run := func(what string, fn func(context.Context) (hostd.Result, error)) tea.Cmd {
		return tea.Batch(note(true, "%s…", what),
			call(fn, func(r hostd.Result, err error) tea.Msg { return hostDone{what, r, err} }))
	}
	switch v.mode {
	case "hostname":
		name := strings.TrimSpace(v.name)
		return run("set hostname", func(ctx context.Context) (hostd.Result, error) { return v.c.SetHostname(ctx, name) })
	case "network":
		n := hostd.Network{Interface: v.iface, Method: v.method}
		if v.method == "static" {
			n.Address, n.Gateway, n.DNS = strings.TrimSpace(v.address), strings.TrimSpace(v.gateway), strings.Fields(v.dns)
		}
		if err := n.Validate(); err != nil {
			return note(false, "network: %v", err)
		}
		return run("change network", func(ctx context.Context) (hostd.Result, error) { return v.c.SetNetwork(ctx, n) })
	case "user":
		name, gh := strings.TrimSpace(v.name), strings.TrimSpace(v.github)
		var keys []string
		for _, l := range strings.Split(v.keys, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				keys = append(keys, l)
			}
		}
		if len(keys) == 0 && gh == "" {
			return note(false, "no keys entered; %s not changed", name)
		}
		return run("set operator "+name, func(ctx context.Context) (hostd.Result, error) { return v.c.SetUser(ctx, name, keys, gh) })
	case "pool":
		disk, serial := v.disk, strings.TrimSpace(v.serial)
		return run("create pool on "+disk+" (this takes a minute)", func(ctx context.Context) (hostd.Result, error) {
			return v.c.CreatePool(ctx, disk, serial)
		})
	case "detach":
		if !v.confirm {
			return note(false, "%s kept", v.name)
		}
		name := v.name
		return run("detach "+name, func(ctx context.Context) (hostd.Result, error) { return v.c.DetachPool(ctx, name) })
	case "delete":
		if !v.confirm {
			return note(false, "%s kept", v.name)
		}
		name := v.name
		return run("delete operator "+name, func(ctx context.Context) (hostd.Result, error) { return v.c.DeleteUser(ctx, name) })
	}
	return nil
}

func (v *hostView) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case hostTick:
		return tea.Batch(v.fetch(), tea.Tick(5*time.Second, func(time.Time) tea.Msg { return hostTick{} }))
	case hostLoaded:
		v.st, v.err = msg.st, msg.err
		if v.cursor >= len(v.st.Users) {
			v.cursor = 0
		}
		return nil
	case hostDone:
		switch {
		case msg.err != nil:
			return tea.Batch(note(false, "%s: %v", msg.what, msg.err), v.fetch())
		case msg.res.Finished == "":
			return note(true, "%s: queued; the host agent has not answered yet", msg.what)
		}
		v.secret = msg.res.Secret
		return tea.Batch(note(true, "%s", msg.res.Message), v.fetch())
	case tea.KeyPressMsg:
		if v.form == nil {
			return v.key(msg.String())
		}
		if msg.String() == "esc" {
			v.form = nil
			return note(false, "changes discarded")
		}
	}
	if v.form == nil {
		return nil
	}
	m, cmd := v.form.Update(msg)
	v.form = m.(*huh.Form)
	switch v.form.State {
	case huh.StateCompleted:
		v.form = nil
		return tea.Batch(cmd, v.submit())
	case huh.StateAborted:
		v.form = nil
	}
	return cmd
}

func (v *hostView) key(k string) tea.Cmd {
	u, ok := v.selected()
	switch k {
	case "up":
		if v.cursor > 0 {
			v.cursor--
		}
	case "down", "j":
		if v.cursor < len(v.st.Users)-1 {
			v.cursor++
		}
	case "h":
		return v.hostnameForm()
	case "n":
		return v.networkForm()
	case "a":
		return v.userForm("")
	case "p":
		return v.poolForm()
	case "x":
		return v.detachForm()
	case "k":
		if ok && u.Managed {
			return v.userForm(u.Name)
		}
	case "d":
		if ok && u.Managed {
			return v.deleteForm(u.Name)
		}
	case "c":
		if v.st.Pending != nil {
			v.mode = ""
			return tea.Batch(note(true, "keeping the network change…"),
				call(v.c.ConfirmNetwork, func(r hostd.Result, err error) tea.Msg { return hostDone{"keep network change", r, err} }))
		}
	}
	return nil
}

func (v *hostView) render(int) string {
	if v.form != nil {
		return v.form.View()
	}
	var b strings.Builder
	if v.err != nil {
		b.WriteString(badSty.Render("API error: "+v.err.Error()) + "\n\n")
	}
	for _, e := range v.st.Errors {
		b.WriteString(badSty.Render("host agent: "+e) + "\n")
	}
	fmt.Fprintf(&b, "Hostname %s\n\n", v.st.Hostname)

	b.WriteString(titleSty.Render("Network") + "\n")
	for _, it := range v.st.Interfaces {
		state := it.State
		if state == "connected" {
			state = goodSty.Render(state)
		} else {
			state = mutedSty.Render(state)
		}
		fmt.Fprintf(&b, "  %-10s %-9s %s %s\n", it.Name, it.Type, it.MAC, state)
		if it.Connection != "" {
			fmt.Fprintf(&b, "             %s %s  gateway %s  dns %s\n", it.Method,
				strings.Join(it.Addresses, " "), orNone(it.Gateway), orNone(strings.Join(it.DNS, " ")))
		}
	}
	if p := v.st.Pending; p != nil {
		left := "?"
		if exp, err := time.Parse(time.RFC3339, p.Expires); err == nil {
			left = strconv.Itoa(max(int(time.Until(exp).Seconds()), 0)) + "s"
		}
		b.WriteString(badSty.Render(fmt.Sprintf("  The change to %s rolls back in %s. Press c to keep it.", p.Interface, left)) + "\n")
	}

	b.WriteString("\n" + titleSty.Render("Model pools") + "\n")
	if len(v.st.Pools) == 0 {
		b.WriteString(mutedSty.Render("  none · press p to make one from an extra disk") + "\n")
	}
	for _, p := range v.st.Pools {
		state := badSty.Render("not mounted")
		if p.Mounted {
			state = goodSty.Render(fmt.Sprintf("%s free of %s", gib(p.Free), gib(p.Size)))
		}
		fmt.Fprintf(&b, "  %-8s %-14s %s\n", p.Name, p.Disk, state)
	}
	if v.secret != "" {
		b.WriteString(badSty.Render("  Recovery key for the new pool. Write it down now, it is not shown again:") + "\n")
		b.WriteString("    " + selSty.Render(v.secret) + "\n")
	}

	b.WriteString("\n" + titleSty.Render("Accounts") + "\n")
	for i, u := range v.st.Users {
		var tags []string
		if u.Admin {
			tags = append(tags, "admin")
		}
		if u.Operator {
			tags = append(tags, "operator")
		}
		if u.Managed {
			tags = append(tags, "created by Sibyl")
		}
		line := fmt.Sprintf("%-16s uid %-6d %d SSH keys  %s", u.Name, u.UID, u.Keys, strings.Join(tags, ", "))
		b.WriteString(cursorLine(i == v.cursor, line) + "\n")
	}
	b.WriteString(mutedSty.Render("  Sibyl manages only the operator accounts that it created. Administrators use sudo.") + "\n")
	if v.st.Updated != "" {
		b.WriteString(mutedSty.Render("  updated "+v.st.Updated) + "\n")
	}
	return b.String()
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
