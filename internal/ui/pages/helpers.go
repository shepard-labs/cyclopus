package pages

// strptr dereferences a nullable store string pointer for rendering.
func strptr(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
