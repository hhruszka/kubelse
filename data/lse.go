package data

import _ "embed"

//go:embed lse.sh
var script []byte

func GetScript() []byte {
	return script
}
