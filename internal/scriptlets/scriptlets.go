package scriptlets

import (
	_ "embed"
	"encoding/json"

	"github.com/LaurensBosscher/read_the_room/internal/rules"
)

//go:embed scriptlets.min.js
var bundle []byte

// JS returns the vendored uBO scriptlet runtime.
func JS() []byte { return bundle }

// Calls returns an inline script that invokes the given scriptlets at
// document-start. It returns "" when there is nothing to invoke. Unknown
// names are skipped at runtime.
func Calls(ss []rules.Scriptlet) string {
	if len(ss) == 0 {
		return ""
	}
	calls := make([][2]any, 0, len(ss))
	for _, s := range ss {
		args := s.Args
		if args == nil {
			args = []string{}
		}
		calls = append(calls, [2]any{s.Name, args})
	}
	payload, err := json.Marshal(calls)
	if err != nil {
		return ""
	}
	return "(function(){var c=" + string(payload) +
		";for(var i=0;i<c.length;i++){try{var f=self.__bp[c[i][0]];if(typeof f==='function')f.apply(undefined,c[i][1]);}catch(e){}}})();"
}
