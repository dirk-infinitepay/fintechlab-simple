package console

import (
	"fmt"
	"strings"
)

// KitEnv is what an environment's kit has that the default's does not
// (docs/lab-api/environments.md §5): its own value for a variable, its own
// content for a download. The lab API builds one from the environment's
// row and applies it over the kits every vendor builds; the default
// environment has none, and gets the kits as they always were.
type KitEnv struct {
	// Env is the environment's id, and Name what it is called.
	Env, Name string
	// Values replace the value of the variable with that key.
	Values map[string]string
	// Files replace the content of the download with that name.
	Files map[string][]byte
	// Lab are the variables the platform reaches the lab API itself with:
	// its address and the environment's own token, for a runner that
	// steers the lab or registers a card.
	Lab []KitVar
}

// Apply returns kits with e's values in place of the default's.
func (e *KitEnv) Apply(kits []Kit) []Kit {
	if e == nil {
		return kits
	}
	out := make([]Kit, 0, len(kits)+1)
	if len(e.Lab) > 0 {
		out = append(out, Kit{Service: "lab", Title: "The lab API",
			Note: fmt.Sprintf("This environment, %s, on the lab API: its token reaches only this environment's data.", e.label()),
			Vars: e.Lab, Files: []KitFile{}})
	}
	for _, k := range kits {
		vars := make([]KitVar, len(k.Vars))
		for i, v := range k.Vars {
			if val, ok := e.Values[v.Key]; ok {
				v.Value, v.Missing = val, ""
				v.Note = ownNote(v.Note)
			}
			vars[i] = v
		}
		files := make([]KitFile, len(k.Files))
		for i, f := range k.Files {
			if _, ok := e.Files[f.Name]; ok {
				f.Present = true
			}
			files[i] = f
		}
		k.Vars, k.Files = vars, files
		out = append(out, k)
	}
	return out
}

// File is the environment's own content for a download, when it has one.
func (e *KitEnv) File(name string) ([]byte, bool) {
	if e == nil {
		return nil, false
	}
	b, ok := e.Files[name]
	return b, ok
}

func (e *KitEnv) label() string {
	if e.Name != "" && e.Name != e.Env {
		return e.Name + " (" + e.Env + ")"
	}
	return e.Env
}

// ownNote rewrites a note that describes the lab's shared login for a
// variable that now carries an environment's own.
func ownNote(note string) string {
	switch {
	case strings.HasPrefix(note, "The lab checks only the password"),
		strings.HasPrefix(note, "Not checked by the lab"):
		return "This environment's own login: the lab knows the environment by it."
	}
	return note
}
