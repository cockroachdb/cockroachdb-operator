package sliceutils

import "slices"

// Filter returns a slice of the elements in sl that pass the predicate.
func Filter[T any](sl []T, pred func(T) bool) []T {
	res := make([]T, 0, len(sl))
	for _, v := range sl {
		if pred(v) {
			res = append(res, v)
		}
	}

	// return the "clipped" slice
	return res[:len(res):len(res)]
}

// Map maps the given slice from type T to type U returning a new slice.
func Map[T, U any](sl []T, fn func(T) U) []U {
	newSl := make([]U, len(sl))
	for i, v := range sl {
		newSl[i] = fn(v)
	}

	return newSl
}

// MapErr maps the given slice from type T to type U returning a new slice.
// Returns an error the first time that `fn` returns an error.
func MapErr[T, U any](sl []T, fn func(T) (U, error)) ([]U, error) {
	newSl := make([]U, len(sl))
	for i, v := range sl {
		res, err := fn(v)
		if err != nil {
			return nil, err
		}
		newSl[i] = res
	}

	return newSl, nil
}

// MapIdx maps the given slice from type T to type U returning a new slice.
// The function is called with the index of the element as the first argument.
func MapIdx[T, U any](sl []T, fn func(int, T) U) []U {
	newSl := make([]U, len(sl))
	for i, v := range sl {
		newSl[i] = fn(i, v)
	}

	return newSl
}

// Flatmap takes a function that returns a slice and applies it to each element
// in the input slice, returning a single slice of the results.
func Flatmap[T, U any](sl []T, fn func(T) []U) []U {
	newSl := make([]U, 0, len(sl))
	for _, v := range sl {
		newSl = append(newSl, fn(v)...)
	}

	return newSl
}

// FlatmapErr takes a function that returns a slice and applies it to each
// element in the input slice, returning a single slice of the results.
// Returns an error the first time that `fn` returns an error.
func FlatmapErr[T, U any](sl []T, fn func(T) ([]U, error)) ([]U, error) {
	newSl := make([]U, 0, len(sl))
	for _, v := range sl {
		res, err := fn(v)
		if err != nil {
			return nil, err
		}
		newSl = append(newSl, res...)
	}

	return newSl, nil
}

// Contains returns whether the given slice contains the given element.
func Contains[T comparable](sl []T, query T) bool {
	return slices.Contains(sl, query)
}

// Batch splits the input slice up into batches.
//
// Batch(2, []int{1, 2, 3, 4, 5}) ->
//
//	[][]int{{1, 2}, {2, 3}, {5}}
func Batch[T any](batchSize int, sl []T) [][]T {
	var batches [][]T
	for len(sl) != 0 {
		end := min(len(sl), batchSize)
		batches = append(batches, sl[0:end])
		sl = sl[end:]
	}
	return batches
}

// Unique returns a single slice of the unique elements from a slice of slices
// using a function to define uniqueness.  The order of the resulting slice is
// maintained from the inputs.
func Unique[T any, S comparable](keyFn func(T) S, slices ...[]T) []T {
	tMap := make(map[S]struct{}, 0)
	uniqueValues := make([]T, 0)
	for _, list := range slices {
		for _, v := range list {
			key := keyFn(v)
			if _, exists := tMap[key]; !exists {
				tMap[key] = struct{}{}
				uniqueValues = append(uniqueValues, v)
			}
		}
	}

	return uniqueValues
}

// Generate returns a slice of the given `size`, filled by calling the `gen`
// function for each index of the slice.
func Generate[T any](size int, gen func(int) T) []T {
	out := make([]T, size)
	for i := range out {
		out[i] = gen(i)
	}
	return out
}

// Partition splits the given slice into two slices, the first containing all
// elements that satisfy the predicate and the second containing all elements
// that do not.
func Partition[T any](ts []T, pred func(T) bool) (pass []T, fail []T) {
	for _, t := range ts {
		if pred(t) {
			pass = append(pass, t)
		} else {
			fail = append(fail, t)
		}
	}
	return pass, fail
}

// MultiSetsEqual returns true if both slices contain the same elements with
// the same counts, regardless of order. [a,a,b] != [a,b,b].
func MultiSetsEqual[T comparable](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[T]int, len(a))
	for _, v := range a {
		counts[v]++
	}
	for _, v := range b {
		counts[v]--
		if counts[v] < 0 {
			return false
		}
	}
	return true
}
