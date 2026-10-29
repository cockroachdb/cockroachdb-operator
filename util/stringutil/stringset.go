package stringutil

import "slices"

// StringSetAdd add the given string to the set of strings if not already
// present. It returns true if the string was added.
// NOTE: The set may be updated to point to a different slice.
func StringSetAdd(set *[]string, s string) bool {
	if slices.Contains(*set, s) {
		return false
	}
	*set = append(*set, s)
	return true
}

// StringSetRemove removes the given string from the set of strings, if it was
// present. It returns true if the string was removed.
// NOTE: The set may be updated to point to a different slice.
func StringSetRemove(set *[]string, s string) bool {
	for i, str := range *set {
		if str == s {
			*set = slices.Delete((*set), i, i+1)
			return true
		}
	}
	return false
}
