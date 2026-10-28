package tui

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/ihavespoons/sibyl/internal/client"
	"github.com/ihavespoons/sibyl/internal/gpu"
	"github.com/ihavespoons/sibyl/internal/hardware"
)

type hardwareLoaded struct {
	hw       hardware.Config
	gpus     []gpu.GPU
	backends []hardware.Backend
	err      error
}

type hardwareSaved struct {
	res client.ApplyResult
	err error
}

// hardwareView is the GPU part of the setup wizard: which GPUs, which
// backend, and the default layout.
type hardwareView struct {
	c       *client.Client
	form    *huh.Form
	backend string
	layout  string
	gpus    []string
	found   []gpu.GPU // detected GPUs, for the summary
	loadErr error
}

func newHardwareView(c *client.Client) *hardwareView { return &hardwareView{c: c} }

func (v *hardwareView) activate() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		hw, err := v.c.Hardware(ctx)
		if err != nil {
			return hardwareLoaded{err: err}
		}
		g, err := v.c.GPUs(ctx)
		return hardwareLoaded{hw: hw, gpus: g, err: err}
	}
}

func (v *hardwareView) capturing() bool { return v.form != nil }

func (v *hardwareView) help() string {
	if v.form != nil {
		return "enter next · shift+tab back · esc discard"
	}
	return "e edit"
}

func (v *hardwareView) setFields(hw hardware.Config) {
	v.backend = string(hw.Backend)
	v.layout = string(hw.Layout)
	v.gpus = nil
	for _, i := range hw.GPUs {
		v.gpus = append(v.gpus, strconv.Itoa(i))
	}
}

func (v *hardwareView) build(msg hardwareLoaded) tea.Cmd {
	v.setFields(msg.hw)
	var opts []huh.Option[string]
	for _, g := range msg.gpus {
		label := fmt.Sprintf("GPU %d  %s  %s  %s", g.Index, g.PCIAddr, gpuModel(g), gib(int64(g.VRAMTotal)))
		opts = append(opts, huh.NewOption(label, strconv.Itoa(g.Index)))
	}
	v.form = huh.NewForm(huh.NewGroup(
		huh.NewMultiSelect[string]().Title("GPUs for inference").
			Description("Leave out a card that drives a display.").
			Options(opts...).Value(&v.gpus).
			// Without a height, huh subtracts the title and description from
			// the height of the options, and two GPUs then get no lines.
			Height(len(opts)+3).
			Validate(func(s []string) error {
				if len(s) == 0 {
					return fmt.Errorf("select at least one GPU")
				}
				return nil
			}),
		huh.NewSelect[string]().Title("Backend").Inline(true).
			Options(backendOptions(msg.backends)...).
			Value(&v.backend),
		huh.NewSelect[string]().Title("Default layout").
			Description("A model can override this in its own settings.").
			Options(huh.NewOption("Span: one model across all selected GPUs", "span"),
				huh.NewOption("Pin: one model per GPU", "pin")).
			Value(&v.layout),
	)).WithShowHelp(false)
	return v.form.Init()
}

// gpuModel is the vendor, and for an AMD GPU its ROCm target and whether
// the HIP image supports it.
func gpuModel(g gpu.GPU) string {
	if g.Arch == "" {
		return g.Vendor
	}
	if !hardware.HIPSupported(g.Arch) {
		return g.Vendor + " " + g.Arch + " (Vulkan only)"
	}
	return g.Vendor + " " + g.Arch
}

// backendNames are the labels for the backends that a host image ships.
var backendNames = map[hardware.Backend]string{
	hardware.BackendHIP:    "HIP (ROCm)",
	hardware.BackendCUDA:   "CUDA",
	hardware.BackendVulkan: "Vulkan",
}

func backendOptions(bs []hardware.Backend) []huh.Option[string] {
	if len(bs) == 0 {
		bs = hardware.Backends(hardware.VariantAMD) // an older System API
	}
	opts := make([]huh.Option[string], 0, len(bs))
	for _, b := range bs {
		opts = append(opts, huh.NewOption(backendNames[b], string(b)))
	}
	return opts
}

func (v *hardwareView) save() tea.Cmd {
	hw := hardware.Config{Backend: hardware.Backend(v.backend), Layout: hardware.Layout(v.layout)}
	for _, s := range v.gpus {
		n, _ := strconv.Atoi(s)
		hw.GPUs = append(hw.GPUs, n)
	}
	return call(func(ctx context.Context) (client.ApplyResult, error) { return v.c.SetHardware(ctx, hw) },
		func(r client.ApplyResult, err error) tea.Msg { return hardwareSaved{r, err} })
}

func (v *hardwareView) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case hardwareLoaded:
		if v.form != nil {
			return nil // do not reset a form that is being edited
		}
		v.loadErr = msg.err
		if msg.err == nil {
			v.setFields(msg.hw)
			v.found = msg.gpus
		}
		return nil
	case hardwareEdit:
		return v.build(msg.hardwareLoaded)
	case hardwareSaved:
		if msg.err != nil {
			return note(false, "save hardware: %v", msg.err)
		}
		text := "hardware saved"
		if msg.res.BackendChanged {
			text += "; llama-swap restarts with the new backend"
		}
		return tea.Batch(note(true, "%s", text), v.activate())
	case tea.KeyPressMsg:
		if v.form == nil {
			if msg.String() == "e" {
				return v.loadAndEdit()
			}
			return nil
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
		return tea.Batch(cmd, v.save())
	case huh.StateAborted:
		v.form = nil
	}
	return cmd
}

// loadAndEdit fetches fresh values, then opens the form.
func (v *hardwareView) loadAndEdit() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		hw, err := v.c.Hardware(ctx)
		if err != nil {
			return noteMsg{"load hardware: " + err.Error(), false}
		}
		g, err := v.c.GPUs(ctx)
		if err != nil {
			return noteMsg{"load GPUs: " + err.Error(), false}
		}
		st, err := v.c.Status(ctx)
		if err != nil {
			return noteMsg{"load status: " + err.Error(), false}
		}
		return hardwareEdit{hardwareLoaded{hw: hw, gpus: g, backends: st.Backends}}
	}
}

type hardwareEdit struct{ hardwareLoaded }

func (v *hardwareView) render(int) string {
	if v.form != nil {
		return v.form.View()
	}
	var b strings.Builder
	if v.loadErr != nil {
		b.WriteString(badSty.Render("API error: "+v.loadErr.Error()) + "\n")
	}
	b.WriteString(titleSty.Render("GPU setup") + "\n\n")
	fmt.Fprintf(&b, "  Backend         %s\n  Default layout  %s\n\n", v.backend, v.layout)
	b.WriteString("  Detected GPUs\n")
	if len(v.found) == 0 {
		b.WriteString(badSty.Render("    none: check that the GPU driver is loaded (lspci -k)") + "\n")
	}
	for _, g := range v.found {
		used := mutedSty.Render("not used")
		if slices.Contains(v.gpus, strconv.Itoa(g.Index)) {
			used = goodSty.Render("used for inference")
		}
		fmt.Fprintf(&b, "    GPU %d  %s  %s  %s  %s\n", g.Index, g.PCIAddr, gpuModel(g), gib(int64(g.VRAMTotal)), used)
	}
	b.WriteString("\n")
	b.WriteString(mutedSty.Render("  Press e to change them. Saving rewrites the llama-swap configuration.") + "\n")
	return b.String()
}
