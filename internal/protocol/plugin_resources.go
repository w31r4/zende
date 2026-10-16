package protocol

import (
	"encoding/hex"
	"regexp"
	"strings"
)

var pluginResourceName = regexp.MustCompile("^[a-z0-9]+(-[a-z0-9]+)*$")

// A stable source selects a logical resource; PluginResourceOrigin identifies
// the exact publication read. The URI is never interpreted as a local path.
func ParsePluginResourceSource(source string) (parent, kind, name string, err error) {
	if len(source) > 4096 {
		return "", "", "", E("invalid_params", "plugin resource source exceeds 4096 bytes")
	}
	parts := strings.Split(strings.TrimPrefix(source, "plugin://"), "/")
	if !strings.HasPrefix(source, "plugin://") || len(parts) != 3 {
		return "", "", "", E("invalid_params", "expected plugin://RESOURCE/skill/NAME or plugin://RESOURCE/prompt/NAME")
	}
	parent, kind, name = parts[0], parts[1], parts[2]
	id, decodeErr := hex.DecodeString(strings.TrimPrefix(parent, "resource_"))
	if decodeErr != nil || len(id) != 16 || parent != "resource_"+hex.EncodeToString(id) || kind != "skill" && kind != "prompt" || len(name) > 64 || !pluginResourceName.MatchString(name) {
		return "", "", "", E("invalid_params", "invalid plugin resource identity")
	}
	return parent, kind, name, nil
}

func (o PluginResourceOrigin) Valid() bool {
	ref := ResourceCatalog{Generation: 1, Entries: []ResourceRef{o.Plugin}}
	digest, err := hex.DecodeString(strings.TrimPrefix(o.DefinitionRef, "plugin_resource_"))
	return ref.Valid() && err == nil && len(digest) == 32 && o.DefinitionRef == "plugin_resource_"+hex.EncodeToString(digest)
}
