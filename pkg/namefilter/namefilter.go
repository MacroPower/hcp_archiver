package namefilter

// Filter is an optional allow-list of names. A nil filter admits every name, so
// an unset filter narrows nothing.
//
// Create instances with [New].
type Filter map[string]struct{}

// New creates a new [Filter] from a list of names, returning nil when the list
// is empty so the zero configuration admits everything.
func New(names []string) Filter {
	if len(names) == 0 {
		return nil
	}

	f := make(Filter, len(names))
	for _, n := range names {
		f[n] = struct{}{}
	}

	return f
}

// Admits reports whether name passes the filter. A nil filter admits every name.
func (f Filter) Admits(name string) bool {
	if f == nil {
		return true
	}

	_, ok := f[name]

	return ok
}
