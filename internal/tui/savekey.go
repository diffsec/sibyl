package tui

// saveKey saves an edit form from any field. huh writes each field's value
// as it changes, so the bound values are current. A field's own check runs
// only when the cursor leaves it, so the System API's check catches a bad
// value that the form skipped.
const saveKey = "ctrl+s"

// formHelp is the help line of an open form. save is true for an edit form
// that saveKey saves. A delete, a confirmation and a disk erase are not
// edit forms: they need their last answer.
func formHelp(save bool) string {
	if save {
		return "enter next · shift+tab back · ctrl+s save · esc discard"
	}
	return "enter next · shift+tab back · esc discard"
}
