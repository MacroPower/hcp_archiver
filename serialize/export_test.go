package serialize

import "maps"

// RedactedFieldNames exposes redactedFieldNames to the external test package,
// so the SDK guard test verifies the same list the safety pass applies.
func RedactedFieldNames() map[string]bool {
	return maps.Clone(redactedFieldNames)
}
