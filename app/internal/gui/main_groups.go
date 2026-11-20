//go:build !android && cgo

package gui

import (
	"slices"

	"clashvless/internal/store"
)

type mainGroup struct {
	indices []int
	mains   []store.Main
}

func mainGroupKey(link string) string {
	return store.MainGroupKey(link)
}

func groupMains(mains []store.Main) []mainGroup {
	var groups []mainGroup
	for _, indices := range store.MainGroups(mains) {
		group := mainGroup{indices: indices}
		for _, i := range indices {
			group.mains = append(group.mains, mains[i])
		}
		groups = append(groups, group)
	}
	return groups
}

func moveMainGroup(slot *store.Slot, mainIndex, delta int) {
	groups := groupMains(slot.Mains)
	for from, group := range groups {
		if !slices.Contains(group.indices, mainIndex) {
			continue
		}
		to := from + delta
		if to < 0 || to >= len(groups) {
			return
		}
		groups[from], groups[to] = groups[to], groups[from]
		var mains []store.Main
		for _, g := range groups {
			mains = append(mains, g.mains...)
		}
		slot.Mains = mains
		return
	}
}
