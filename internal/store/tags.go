package store

import (
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Tags are words in a task's title or description that start with a #, as in
// "Buy adapters #shopping". There is nothing to create or manage: a tag exists
// while some task mentions it. They match whatever their case, so #Shopping
// and #shopping are the same tag.

// TagMark is where a tag appears in some text: the byte range of "#word", and
// the tag it names, lower-cased and without the #.
type TagMark struct {
	Start, End int
	Tag        string
}

// FindTags returns the tags in text, in order. A # only starts a tag at the
// beginning of a word, so "C#" and "example.com/#top" are not tags, and the
// tag must have a letter in it, so "issue #12" is not one either.
func FindTags(text string) []TagMark {
	var marks []TagMark
	for i := 0; i < len(text); {
		j := strings.IndexByte(text[i:], '#')
		if j < 0 {
			break
		}
		start := i + j
		i = start + 1
		if before, _ := utf8.DecodeLastRuneInString(text[:start]); start > 0 && !startsWord(before) {
			continue
		}
		end, letter := i, false
		for end < len(text) {
			r, n := utf8.DecodeRuneInString(text[end:])
			if !inTag(r) {
				break
			}
			letter = letter || unicode.IsLetter(r)
			end += n
		}
		// "#trip-" ends a sentence rather than naming the tag "trip-".
		for end > i && (text[end-1] == '-' || text[end-1] == '_') {
			end--
		}
		if !letter || end == i {
			continue
		}
		marks = append(marks, TagMark{Start: start, End: end, Tag: strings.ToLower(text[i:end])})
		i = end
	}
	return marks
}

// startsWord reports whether a # after r can begin a tag.
func startsWord(r rune) bool {
	return !inTag(r) && r != '#' && r != '/' && r != '&'
}

func inTag(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r) || r == '_' || r == '-'
}

// NormalizeTag turns what someone typed ("#Shopping", "shopping") into the tag
// it names, and reports whether it is one.
func NormalizeTag(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "#") {
		s = "#" + s
	}
	marks := FindTags(s)
	if len(marks) != 1 || marks[0].Start != 0 || marks[0].End != len(s) {
		return "", false
	}
	return marks[0].Tag, true
}

// TagsIn is every tag the texts mention (a task's title and description),
// sorted and without repeats.
func TagsIn(texts ...string) []string {
	var tags []string
	seen := map[string]bool{}
	for _, text := range texts {
		for _, m := range FindTags(text) {
			if !seen[m.Tag] {
				seen[m.Tag] = true
				tags = append(tags, m.Tag)
			}
		}
	}
	sort.Strings(tags)
	return tags
}

// HasTag reports whether the task mentions tag (as NormalizeTag returns it).
func (t Task) HasTag(tag string) bool { return slices.Contains(t.Tags, tag) }

// WithTag keeps the tasks that mention tag, reusing the slice.
func WithTag(tasks []Task, tag string) []Task {
	return slices.DeleteFunc(tasks, func(t Task) bool { return !t.HasTag(tag) })
}

// TagCount is how many tasks mention a tag.
type TagCount struct {
	Tag   string `json:"tag"`
	Tasks int    `json:"tasks"`
}

// CountTags lists the tags the given tasks use, alphabetically.
func CountTags(tasks []Task) []TagCount {
	n := map[string]int{}
	for _, t := range tasks {
		for _, tag := range t.Tags {
			n[tag]++
		}
	}
	counts := make([]TagCount, 0, len(n))
	for tag, c := range n {
		counts = append(counts, TagCount{tag, c})
	}
	sort.Slice(counts, func(i, j int) bool { return counts[i].Tag < counts[j].Tag })
	return counts
}
