package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResourceCatalogValidation(t *testing.T) {
	ref := ResourceRef{ID: "resource_" + strings.Repeat("a", 32), Version: strings.Repeat("b", 64)}
	for name, test := range map[string]struct {
		catalog ResourceCatalog
		valid   bool
	}{
		"empty":            {ResourceCatalog{}, true},
		"ready":            {ResourceCatalog{Generation: 5, Entries: []ResourceRef{ref}}, true},
		"negative":         {ResourceCatalog{Generation: -1}, false},
		"empty generation": {ResourceCatalog{Entries: []ResourceRef{ref}}, false},
		"missing entries":  {ResourceCatalog{Generation: 5}, false},
		"duplicate":        {ResourceCatalog{Generation: 5, Entries: []ResourceRef{ref, ref}}, false},
		"not digest":       {ResourceCatalog{Generation: 5, Entries: []ResourceRef{{ID: ref.ID, Version: "latest"}}}, false},
		"not identity":     {ResourceCatalog{Generation: 5, Entries: []ResourceRef{{ID: strings.TrimPrefix(ref.ID, "resource_"), Version: ref.Version}}}, false},
		"uppercase":        {ResourceCatalog{Generation: 5, Entries: []ResourceRef{{ID: ref.ID, Version: strings.ToUpper(ref.Version)}}}, false},
		"unsorted":         {ResourceCatalog{Generation: 5, Entries: []ResourceRef{ref, {ID: "resource_" + strings.Repeat("0", 32), Version: ref.Version}}}, false},
		"oversized":        {ResourceCatalog{Generation: 5, Entries: make([]ResourceRef, MaxAgentResources+1)}, false},
	} {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(test.catalog)
			if err != nil {
				t.Fatal(err)
			}
			var copy ResourceCatalog
			if err := StrictDecode(data, &copy); err != nil || copy.Valid() != test.valid {
				t.Fatal(string(data), copy.Valid(), err)
			}
		})
	}
}
