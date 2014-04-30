package graphdb

// Returns the depth or number of / in a given path
func _PathDepth(p string) (ret int) {
	var (
		length = len(p)
		n      = 0
	)
	for i := 0; i < length; i++ {
		if p[i] == '/' {
			n++
		}
	}
	if n == 1 && length == 1 {
		return 1
	}
	return n + 1
}

func sortByDepth(paths []string) {
	sorted := false
	for !sorted {
		sorted = true
		for i := 0; i < len(paths)-1; i++ {
			if _PathDepth(paths[i]) < _PathDepth(paths[i+1]) {
				paths[i], paths[i+1] = paths[i+1], paths[i]
				sorted = false
			}
		}
	}
}
