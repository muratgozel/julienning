// Package tui is the session picker: a pure Model (items, filter, cursor)
// with Update and View, driven either by a raw-mode terminal loop or by a
// numbered-prompt fallback. Terminal IO lives in pick.go; everything here is
// deterministic and tested without a TTY.
//
// Accessibility: the selection is always marked with a ">" character in
// addition to reverse video, colour is never the only signal, NO_COLOR turns
// every SGR sequence off, and the terminal cursor is parked on the selected
// row so screen readers and magnifiers follow it.
package tui

import (
	"fmt"
	"strings"
)

// Item is one picker row.
type Item struct {
	Title    string // first line
	Meta     string // appended to the first line, e.g. "julienning3 · 2h ago · 1a2b3c4d"
	Note     string // status after Meta, e.g. "(open in another terminal)"
	Detail   string // optional second line, e.g. the first prompt
	Search   string // extra text the filter matches (ids, config names)
	Disabled bool   // shown dim; cannot be chosen
	Muted    bool   // shown dim but can be chosen, e.g. "(may be open)"
	Pinned   bool   // always visible, whatever the filter
}

// Model is the picker state. The zero Cursor selects the first visible
// selectable row. Items must not change after New: their filter text is
// precomputed there.
type Model struct {
	Items  []Item
	Header string
	// Scope says what is listed, e.g. "100 most recent sessions in
	// ~/Code/shop"; it takes the filter line's place while no filter is
	// typed. Empty shows the plain "Filter: (type to search)" prompt.
	Scope   string
	Filter  string
	Cursor  int // position in Visible()
	NoColor bool

	Done     bool // Enter on a selectable row
	Canceled bool // Esc / Ctrl-C

	// typing: '/' was pressed or a filter is being typed, so j/k are
	// letters rather than navigation.
	typing bool
	offset int // first visible row index (scrolling), see Layout
	height int // terminal height from the last Layout; 0 before it
	// hay is each item's lower-cased match text, built once by New so a
	// keystroke never re-lowercases hundreds of rows.
	hay []string
}

// New returns a model with the cursor on the first selectable row.
func New(items []Item, header string) Model {
	m := Model{Items: items, Header: header, hay: make([]string, len(items))}
	for i, it := range items {
		m.hay[i] = hayOf(it)
	}
	m.Cursor = m.firstSelectable(false)
	return m
}

// Selected returns the index into Items of the row under the cursor.
func (m Model) Selected() (int, bool) {
	vis := m.Visible()
	if m.Cursor < 0 || m.Cursor >= len(vis) {
		return 0, false
	}
	i := vis[m.Cursor]
	return i, !m.Items[i].Disabled
}

// Visible returns the indexes of the items matching the filter. Every
// space-separated word must appear (case-insensitive) in the title, detail,
// meta or search text.
func (m Model) Visible() []int {
	words := strings.Fields(strings.ToLower(m.Filter))
	out := make([]int, 0, len(m.Items))
	for i, it := range m.Items {
		if it.Pinned || len(words) == 0 || matches(m.haystack(i), words) {
			out = append(out, i)
		}
	}
	return out
}

func hayOf(it Item) string {
	return strings.ToLower(it.Title + "\n" + it.Detail + "\n" + it.Meta + "\n" + it.Search)
}

// haystack is Items[i]'s match text; a Model built without New (as
// Fallback does) computes it on the fly.
func (m Model) haystack(i int) string {
	if len(m.hay) == len(m.Items) {
		return m.hay[i]
	}
	return hayOf(m.Items[i])
}

func matches(hay string, words []string) bool {
	for _, w := range words {
		if !strings.Contains(hay, w) {
			return false
		}
	}
	return true
}

// Update applies one key press.
func (m Model) Update(k Key) Model {
	if m.Done || m.Canceled {
		return m
	}
	switch k.Kind {
	case KeyUp:
		m.Cursor = m.step(-1, 1)
	case KeyDown:
		m.Cursor = m.step(1, 1)
	case KeyPageUp:
		m.Cursor = m.page(-1)
	case KeyPageDown:
		m.Cursor = m.page(1)
	case KeyHome:
		m.Cursor = m.edge(false)
	case KeyEnd:
		m.Cursor = m.edge(true)
	case KeyEnter:
		if _, ok := m.Selected(); ok {
			m.Done = true
		}
	case KeyEsc, KeyInterrupt:
		m.Canceled = true
	case KeyBackspace:
		if m.Filter == "" {
			m.typing = false
			break
		}
		rs := []rune(m.Filter)
		m.setFilter(string(rs[:len(rs)-1]))
	case KeyClearFilter:
		m.typing = false
		m.setFilter("")
	case KeyRune:
		switch {
		case !m.typing && m.Filter == "" && k.Rune == 'j':
			m.Cursor = m.step(1, 1)
		case !m.typing && m.Filter == "" && k.Rune == 'k':
			m.Cursor = m.step(-1, 1)
		case !m.typing && m.Filter == "" && k.Rune == '/':
			m.typing = true
		case !m.typing && m.Filter == "" && k.Rune == ' ':
			// a leading space would filter nothing; ignore it
		default:
			m.typing = true
			m.setFilter(m.Filter + string(k.Rune))
		}
	}
	return m
}

func (m *Model) setFilter(f string) {
	m.Filter = f
	m.offset = 0
	m.Cursor = m.firstSelectable(f != "")
}

// firstSelectable returns the first selectable visible row; with
// preferMatch, a filtered (non-pinned) match wins over pinned rows so Enter
// after typing picks what was searched for.
func (m Model) firstSelectable(preferMatch bool) int {
	vis := m.Visible()
	if preferMatch {
		for pos, i := range vis {
			if !m.Items[i].Pinned && !m.Items[i].Disabled {
				return pos
			}
		}
	}
	for pos, i := range vis {
		if !m.Items[i].Disabled {
			return pos
		}
	}
	return 0
}

// step moves n selectable rows in dir, stopping at the ends.
func (m Model) step(dir, n int) int {
	vis := m.Visible()
	cur := m.Cursor
	for moved := 0; moved < n; {
		next := cur + dir
		for next >= 0 && next < len(vis) && m.Items[vis[next]].Disabled {
			next += dir
		}
		if next < 0 || next >= len(vis) {
			break
		}
		cur = next
		moved++
	}
	return cur
}

// page returns where PgUp (dir -1) or PgDn (dir 1) lands: the farthest
// selectable row that still fits on one screen together with the cursor row,
// so the old cursor row stays in view as the first or last row. When every
// such row is disabled it goes on to the next selectable row beyond them.
// Before the first Layout the height is unknown and a page is 5 rows.
func (m Model) page(dir int) int {
	if m.height <= 0 {
		return m.step(dir, 5)
	}
	vis := m.Visible()
	if m.Cursor < 0 || m.Cursor >= len(vis) {
		return m.Cursor
	}
	avail := max(m.height-chromeLines, 1)
	used := itemHeight(m.Items[vis[m.Cursor]])
	pos := m.Cursor
	for next := pos + dir; next >= 0 && next < len(vis); next += dir {
		if used += itemHeight(m.Items[vis[next]]); used > avail {
			break
		}
		pos = next
	}
	if pos == m.Cursor {
		// Not even the neighbour fits beside the cursor row (tiny screen).
		return m.step(dir, 1)
	}
	for p := pos; p != m.Cursor; p -= dir {
		if !m.Items[vis[p]].Disabled {
			return p
		}
	}
	for p := pos + dir; p >= 0 && p < len(vis); p += dir {
		if !m.Items[vis[p]].Disabled {
			return p
		}
	}
	return m.Cursor
}

func (m Model) edge(last bool) int {
	vis := m.Visible()
	if !last {
		return m.firstSelectable(false)
	}
	for pos := len(vis) - 1; pos >= 0; pos-- {
		if !m.Items[vis[pos]].Disabled {
			return pos
		}
	}
	return m.Cursor
}

// chromeLines is the header, filter and help lines around the list.
const chromeLines = 3

func itemHeight(it Item) int {
	if it.Detail != "" {
		return 2
	}
	return 1
}

// Layout scrolls the list so the cursor row is visible in a terminal of the
// given height, moving as little as possible, and remembers the height for
// PgUp/PgDn. The run loop calls it before every key, so a resize is picked
// up; View calls it too. It is linear in the number of rows.
func (m Model) Layout(height int) Model {
	m.height = height
	vis := m.Visible()
	if len(vis) == 0 {
		m.offset = 0
		return m
	}
	avail := max(height-chromeLines, 1)
	h := func(pos int) int { return itemHeight(m.Items[vis[pos]]) }
	cur := min(max(m.Cursor, 0), len(vis)-1)
	// Above the window: the cursor row becomes the first one.
	m.offset = min(max(m.offset, 0), cur)
	// Below it: scroll just far enough to show the whole cursor row last.
	used, lo := h(cur), cur
	for lo > 0 && used+h(lo-1) <= avail {
		lo--
		used += h(lo)
	}
	m.offset = max(m.offset, lo)
	// No blank lines under the last row while rows above it are hidden
	// (after End, or when the terminal grows).
	used, top := 0, len(vis)
	for top > 0 && used+h(top-1) <= avail {
		top--
		used += h(top)
	}
	m.offset = min(m.offset, top)
	return m
}

// SGR sequences; dropped entirely under NO_COLOR. Plain SGR 2 (faint) is
// used rather than a 256-colour grey: it needs no capability detection and
// every terminal that ignores it still shows the text.
const (
	sgrReverse = "\x1b[7m"
	sgrDim     = "\x1b[2m"
	sgrReset   = "\x1b[0m"
)

// tone is how a line segment is drawn.
type tone int

const (
	toneNormal  tone = iota
	toneDim          // secondary text: metadata, prompts, chrome, unavailable rows
	toneReverse      // the selected row's first line
)

// paint wraps s in t's SGR sequence. Under NoColor (and for toneNormal) it
// returns s unchanged, so plain output never carries an escape.
func (m Model) paint(s string, t tone) string {
	if m.NoColor || s == "" {
		return s
	}
	switch t {
	case toneDim:
		return sgrDim + s + sgrReset
	case toneReverse:
		return sgrReverse + s + sgrReset
	}
	return s
}

// detailIndent prefixes an item's second line, so the hierarchy survives
// without colour.
const detailIndent = "    "

// minTitle is the narrowest a title is squeezed (in columns) to keep the
// metadata after it whole; below that the metadata gives way instead.
const minTitle = 12

// View renders the picker as lines separated by "\n", none wider than
// width-1 columns (a line that fills the last column makes some terminals
// wrap), at most height lines. It does not clear the screen or position the
// cursor; see CursorLine.
func (m Model) View(width, height int) string {
	lines, _ := m.render(width, height)
	return strings.Join(lines, "\n")
}

// CursorLine is the 0-based line of View's output holding the selection
// marker, or -1 when nothing is selectable.
func (m Model) CursorLine(width, height int) int {
	_, at := m.render(width, height)
	return at
}

func (m Model) render(width, height int) ([]string, int) {
	w := max(width, 20) - 1
	height = max(height, chromeLines+1)
	m = m.Layout(height)
	var lines []string
	lines = append(lines, Truncate(sanitize(m.Header), w))

	vis := m.Visible()
	matched, total := 0, 0
	for _, it := range m.Items {
		if !it.Pinned {
			total++
		}
	}
	for _, i := range vis {
		if !m.Items[i].Pinned {
			matched++
		}
	}
	// The label, placeholder and counts are chrome; typed filter text stays
	// normal so what is being edited is easy to read.
	switch {
	case m.Filter != "":
		const label = "Filter: "
		filter := sanitize(m.Filter)
		count := fmt.Sprintf(" · %d of %d", matched, total)
		room := w - Width(label)
		if Width(filter)+Width(count) > room {
			count = "" // the text being typed matters more than the count
		}
		lines = append(lines, m.paint(label, toneDim)+Truncate(filter, room)+m.paint(count, toneDim))
	case m.Scope != "":
		lines = append(lines, m.paint(Truncate(sanitize(m.Scope)+" · type to filter", w), toneDim))
	default:
		lines = append(lines, m.paint(Truncate("Filter: (type to search)", w), toneDim))
	}

	avail := height - chromeLines
	cursorAt := -1
	pos := m.offset
	for ; pos < len(vis) && avail > 0; pos++ {
		it := m.Items[vis[pos]]
		selected := pos == m.Cursor && !it.Disabled
		if selected {
			cursorAt = len(lines)
		}
		lines = append(lines, m.row(it, selected, w))
		avail--
		if it.Detail != "" && avail > 0 {
			// Dim even under a selected first line: reverse video marks the
			// row, the prompt stays secondary.
			detail := Truncate(sanitize(it.Detail), w-Width(detailIndent))
			lines = append(lines, m.paint(detailIndent+detail, toneDim))
			avail--
		}
	}
	if m.Filter != "" && matched == 0 && avail > 0 {
		lines = append(lines, m.paint(Truncate("  (no matches)", w), toneDim))
	}

	move := "↑/↓ move"
	if m.offset > 0 || pos < len(vis) {
		move = "↑/↓ PgUp/PgDn move" // more rows than fit: say how to page
	}
	help := move + " · type to filter · Enter select · Esc cancel"
	if m.typing || m.Filter != "" {
		help = move + " · Backspace edit filter · Enter select · Esc cancel"
	}
	lines = append(lines, m.paint(Truncate(help, w), toneDim))
	return lines, cursorAt
}

// row renders an item's first line in w columns: marker and title in the
// normal tone, metadata and note dim. The selected row is reverse video as a
// whole (faint inside reverse renders inconsistently across terminals);
// disabled and muted rows are dim as a whole.
func (m Model) row(it Item, selected bool, w int) string {
	marker := "  "
	if selected {
		marker = "> "
	}
	title, meta, note := fitRow(sanitize(it.Title), sanitize(it.Meta), sanitize(it.Note), w-Width(marker))
	head := marker + title
	var tail string
	if meta != "" {
		tail += "  " + meta
	}
	if note != "" {
		tail += " " + note
	}
	switch {
	case selected:
		return m.paint(head+tail, toneReverse)
	case it.Disabled || it.Muted:
		return m.paint(head+tail, toneDim)
	}
	return head + m.paint(tail, toneDim)
}

// fitRow fits a first line's parts into room columns, laid out as
// "title  meta note", and returns them truncated. The note carries meaning
// colour alone must not (e.g. "open in another terminal"), so it is kept
// whole; then the metadata, with the title giving way first down to
// minTitle columns; only then is the metadata cut, and it is dropped when
// fewer than 4 columns of it would show.
func fitRow(title, meta, note string, room int) (string, string, string) {
	if note != "" {
		if Width(note)+1 > room-10 {
			// Too narrow to keep both apart: lead with the note.
			return Truncate(note+" "+title, room), "", ""
		}
		room -= Width(note) + 1
	}
	if meta == "" {
		return Truncate(title, room), "", note
	}
	const gap = 2
	keep := min(Width(title), minTitle)
	switch metaRoom := room - keep - gap; {
	case Width(meta) <= metaRoom:
		return Truncate(title, room-gap-Width(meta)), meta, note
	case metaRoom >= 4:
		return Truncate(title, keep), Truncate(meta, metaRoom), note
	}
	return Truncate(title, room), "", note
}
