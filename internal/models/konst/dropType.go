package konst

// DropTypeMap maps an API drop type to a database drop type.
// The map must not be modified.
var DropTypeMap = map[string]string{
	"REGULAR_DROP": "REGULAR",
	"NORMAL_DROP":  "REGULAR",
	"SPECIAL_DROP": "SPECIAL",
	"EXTRA_DROP":   "EXTRA",
}

var DropTypeMapKeys = []string{
	"NORMAL_DROP",
	"SPECIAL_DROP",
	"EXTRA_DROP",
}
