package slit

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"strconv"
	"sync"
	"time"

	"code.cloudfoundry.org/bytefmt"
	"github.com/mattn/go-runewidth"
	"github.com/nsf/termbox-go"
	"github.com/oderwat/slit/ansi"
	"github.com/oderwat/slit/filters"
	"github.com/oderwat/slit/logging"
	"github.com/oderwat/slit/utils"
)

type viewer struct {
	hOffset       int
	width         int
	height        int
	sizeLock      sync.Mutex
	wrap          bool
	fetcher       *Fetcher
	focus         Focusing
	info          infobar
	forwardSearch bool
	search        []rune
	buffer        viewBuffer
	keepChars     int
	ctx           context.Context
	following     bool
}

type action uint

const (
	NO_ACTION action = iota
	ACTION_QUIT
	ACTION_RESET_FOCUS
)

type View interface {
}

type Focusing interface {
	View
	processKey(ev termbox.Event) action
}

type Navigator interface {
	Focusing
	navigate(direction int)
}

func (v *viewer) searchForward() {
	searchFunc, err := filters.GetSearchFunc(v.info.searchType, v.search)
	if err != nil {
		return
	}
	if distance := v.buffer.searchForward(searchFunc); distance != -1 {
		v.navigate(distance)
		return
	}
	if pos := v.fetcher.Search(context.TODO(), v.buffer.lastLine().Pos, searchFunc); pos != POS_NOT_FOUND {
		v.buffer.reset(pos)
		v.draw()
	} else {
		v.info.setMessage(ibMessage{str: fmt.Sprintf("'%s' not found", string(v.search)), color: termbox.ColorRed})
	}
}

func (v *viewer) searchHighlighted() {
	if distance := v.buffer.searchForwardHighlighted(); distance != -1 {
		v.navigate(distance)
		return
	}
	if pos := v.fetcher.SearchHighlighted(context.TODO(), v.buffer.lastLine().Pos); pos != POS_NOT_FOUND {
		v.buffer.reset(pos)
		v.draw()
	}
}

func (v *viewer) searchBack() {
	searchFunc, err := filters.GetSearchFunc(v.info.searchType, v.search)
	if err != nil {
		return
	}
	if distance := v.buffer.searchBack(searchFunc); distance != -1 {
		v.navigate(-distance)
		return
	}
	fromPos := v.buffer.currentLine().Pos
	if fromPos.Line > 0 {
		fromPos.Line--
	}
	fromPos.Offset--
	if pos := v.fetcher.SearchBack(context.TODO(), fromPos, searchFunc); pos != POS_NOT_FOUND {
		v.buffer.reset(pos)
		v.draw()
	} else {
		v.info.setMessage(ibMessage{str: fmt.Sprintf("'%s' not found", string(v.search)), color: termbox.ColorRed})
	}
}

func (v *viewer) searchBackHighlighted() {
	if distance := v.buffer.searchBackHighlighted(); distance != -1 {
		v.navigate(-distance)
		return
	}
	fromPos := v.buffer.currentLine().Pos
	if fromPos.Line > 0 {
		fromPos.Line--
	}
	fromPos.Offset--
	if pos := v.fetcher.SearchBackHighlighted(context.TODO(), fromPos); pos != POS_NOT_FOUND {
		v.buffer.reset(pos)
		v.draw()
	}
}

func (v *viewer) nextSearch(reverse bool) {
	if len(v.search) == 0 {
		return
	}
	if v.forwardSearch != reverse { // Basically XOR
		v.searchForward()
	} else {
		v.searchBack()
	}
}

func (v *viewer) applyFilter(filter *filters.Filter) {
	v.fetcher.lock.Lock()
	v.fetcher.filters = append(v.fetcher.filters, filter)
	v.fetcher.filtersEnabled = true
	v.buffer.reset(v.buffer.currentLine().Pos)
	v.fetcher.lock.Unlock()
}

func (v *viewer) addFilter(sub []rune, action filters.FilterAction) {
	filter, err := filters.NewFilter(sub, action, v.info.searchType)
	if err != nil {
		logging.Debug(err)
		return
	}
	v.applyFilter(filter)
}

func (v *viewer) switchFilters() {
	v.fetcher.filtersEnabled = !v.fetcher.filtersEnabled
	v.buffer.reset(v.buffer.currentLine().Pos)
	v.draw()
}

var stylesMap = map[uint8]termbox.Attribute{
	1: termbox.AttrBold,
	7: termbox.AttrReverse,
}

func (v *viewer) replaceWithKeptChars(data ansi.Astring) ([]rune, []ansi.RuneAttr) {
	dataLen := len(data.Runes)
	if v.keepChars <= 0 || v.wrap {
		sliceFromIdx := utils.Min(v.hOffset, dataLen)
		return data.Runes[sliceFromIdx:], data.Attrs[sliceFromIdx:]
	}

	var chars []rune
	var attrs []ansi.RuneAttr

	if dataLen > v.keepChars {
		chars = make([]rune, v.keepChars, dataLen)
		attrs = make([]ansi.RuneAttr, v.keepChars, dataLen)
		copy(chars, data.Runes[:v.keepChars])
		copy(attrs, data.Attrs[:v.keepChars])

		rightSliceBegin := utils.Min(v.keepChars+v.hOffset, dataLen)
		chars = append(chars, data.Runes[rightSliceBegin:]...)
		attrs = append(attrs, data.Attrs[rightSliceBegin:]...)
	} else {
		chars = make([]rune, dataLen)
		attrs = make([]ansi.RuneAttr, dataLen)
		copy(chars, data.Runes)
		copy(attrs, data.Attrs)
	}
	for i := 0; i < v.keepChars && i < len(chars); i++ {
		attr := &attrs[i]
		attr.Fg = ansi.FgColor(ansi.ColorBlue)
		//attr.Bg = ansi.BgColor(ansi.ColorBlue)
		//attr.Style = uint8(ansi.StyleBold)
	}
	return chars, attrs
}

func ToTermboxAttr(attr ansi.RuneAttr) (fg, bg termbox.Attribute) {
	style := stylesMap[attr.Style]

	// For "standard" 3-bit colors, convert to termbox attribute value (0-7)
	// If bold attribute is set, shift the color value +8 for high intensity
	// color AND continue to set the bold attribute before returning
	if attr.Fg >= 30 && attr.Fg <= 37 {
		fg = termbox.Attribute(attr.Fg - 30 + 1)
		if style == termbox.AttrBold {
			fg = fg | 1<<3
		}
	}
	if attr.Bg >= 40 && attr.Bg <= 47 {
		bg = termbox.Attribute(attr.Bg - 40 + 1)
		if style == termbox.AttrBold {
			bg = bg | 1<<3
		}
	}

	// if none of the above conditions were matched, the attr color values are
	// either 0 or a specific color code between 16-255, in which case we can
	// use them unaltered
	if fg == 0 {
		fg = termbox.Attribute(attr.Fg)
	}
	if bg == 0 {
		bg = termbox.Attribute(attr.Bg)
	}

	fg = fg | style

	return fg, bg
}

func (v *viewer) draw() {
	termbox.Clear(termbox.ColorDefault, termbox.ColorDefault)
	var chars []rune
	var attrs []ansi.RuneAttr
	var attr ansi.RuneAttr
	var highlightStyle termbox.Attribute
	var hlIndices [][]int
	var hlChars int
	var tx int
	for ty, dataLine := 0, 0; ty < v.height; ty++ {
		tx = 0
		hlChars = 0
		line, err := v.buffer.getLine(dataLine)
		if err == io.EOF {
			break
		}
		chars, attrs = v.replaceWithKeptChars(line.Str)
		hlIndices = [][]int{}
		if len(v.search) != 0 {
			searchFunc, err := filters.GetSearchFunc(v.info.searchType, v.search)
			if err == nil {
				hlIndices = filters.IndexAll(searchFunc, chars)
			}
		}
		for i, char := range chars {
			repeat := 1
			attr = attrs[i]
			highlightStyle = termbox.Attribute(0)
			if len(hlIndices) != 0 && hlChars == 0 {
				if hlIndices[0][0] == i {
					hlChars = hlIndices[0][1] - hlIndices[0][0]
					hlIndices = hlIndices[1:]
				}
			}
			if hlChars != 0 {
				highlightStyle = termbox.AttrReverse
				hlChars--
			}
			if line.Highlighted {
				//highlightStyle = highlightStyle | termbox.AttrUnderline | termbox.AttrBold
				attr.Bg = attr.Bg | ansi.BgColor(ansi.ColorYellow)
			}

			if char == 9 { // TAB
				char = '·' // 0x0387
				attr.Fg = attr.Fg | ansi.FgColor(ansi.ColorGray)
				repeat = 4
			}

			if char == 92 { // PARAGRAPH SEPARATOR (which is what we get for a tab in the input data
				char = '»'
				attr.Fg = attr.Fg | ansi.FgColor(ansi.ColorGray)
				repeat = 4
			}

			fg, bg := ToTermboxAttr(attr)

			if highlightStyle != termbox.Attribute(0) {
				fg = fg | highlightStyle
			}

			breakIt := false
			for repeat > 0 {
				repeat--
				termbox.SetCell(tx, ty, char, fg, bg)
				tx += runewidth.RuneWidth(char)
				if tx >= v.width {
					if v.wrap {
						tx = 0
						ty++
					} else {
						breakIt = true
						break
					}
				}
			}
			if breakIt {
				break
			}
		}
		if ty >= v.height {
			break
		}
		dataLine++
	}
	termbox.Flush()
}

func (v *viewer) navigate(direction int) {
	v.buffer.shift(direction)
	v.following = false
	if config.follow && !v.buffer.isFull() {
		v.following = true
	}
	v.draw()
}

func (v *viewer) navigateEnd() {
	v.buffer.reset(Pos{POS_UNKNOWN, v.fetcher.lastOffset()})
	v.navigate(-v.height) //not adding +1 since nothing on screen now
	if config.follow {
		v.following = true
	}
}

func (v *viewer) navigateStart() {
	v.following = false
	v.buffer.reset(Pos{0, 0})
	v.draw()
}

func (v *viewer) navigateHorizontally(direction int) {
	v.wrap = false
	v.hOffset += direction
	if v.hOffset < 0 {
		v.hOffset = 0
	}
	v.draw()
}

func (v *viewer) navigateRight() {
	v.navigateHorizontally(v.width / 2)
}

func (v *viewer) navigateLeft() {
	v.navigateHorizontally(-v.width / 2)
}

func (v *viewer) resetFocus() {
	v.focus = v
	termbox.HideCursor()
	termbox.Flush()
}

func (v *viewer) onUserAction() {
	v.info.reset(ibModeStatus)
}

func (v *viewer) processKey(ev termbox.Event) (a action) {
	v.onUserAction()
	if ev.Ch != 0 {
		switch ev.Ch {
		case 'w':
			logging.Debug("switching wrapping")
			v.wrap = !v.wrap
			if v.wrap {
				v.hOffset = 0
			}
			v.draw()
		case 'q':
			logging.Debug("got key quit")
			return ACTION_QUIT
		case 'n':
			v.nextSearch(false)
		case 'N':
			v.nextSearch(true)
		case 'h':
			v.searchHighlighted()
		case 'H':
			v.searchBackHighlighted()
		case 'U':
			if ok := v.fetcher.removeLastFilter(); ok {
				v.buffer.refresh()
				v.draw()
			}
		case 'g':
			v.navigateStart()
		case 'G':
			v.navigateEnd()
		case 'f':
			v.navigatePageDown()
		case 'b':
			v.navigatePageUp()
		case '/', 's':
			v.focus = &v.info
			v.info.reset(ibModeSearch)
		case filters.FilterIntersectChar:
			v.focus = &v.info
			v.info.reset(ibModeFilter)
		case filters.FilterUnionChar:
			v.focus = &v.info
			v.info.reset(ibModeAppend)
		case filters.FilterExcludeChar:
			v.focus = &v.info
			v.info.reset(ibModeExclude)
		case filters.FilterHighlightChar:
			v.focus = &v.info
			v.info.reset(ibModeHighlight)
		case '°', 'm':
			v.fetcher.toggleHighlight(v.buffer.currentLine().Pos.Line)
			v.buffer.toggleCurrentHighlight()
			v.draw()
		case '?', 'S':
			v.focus = &v.info
			v.info.reset(ibModeBackSearch)
		case 'M':
			reportSystemUsage()
		case '=':
			v.dropFilters()
		case 'C':
			v.switchFilters()
		case 'K':
			v.focus = &v.info
			v.info.reset(ibModeKeepCharacters)
		case 'j':
			v.navigate(+1)
		case 'k':
			v.navigate(-1)
		case '<', 'ä':
			v.navigateHorizontally(+1)
		case '>', 'ö':
			v.navigateHorizontally(-1)

		}
	} else {
		switch ev.Key {
		case termbox.KeyArrowDown:
			v.navigate(+1)
		case termbox.KeyArrowUp:
			v.navigate(-1)
		case termbox.KeyArrowRight:
			v.navigateRight()
		case termbox.KeyArrowLeft:
			v.navigateLeft()
		case termbox.KeyCtrlB, termbox.KeyPgup:
			v.navigatePageUp()
		case termbox.KeyCtrlU:
			v.navigateHalfPageUp()
		case termbox.KeyCtrlF, termbox.KeyPgdn, termbox.KeySpace:
			v.navigatePageDown()
		case termbox.KeyCtrlD:
			v.navigateHalfPageDown()
		case termbox.KeyHome:
			v.navigateStart()
		case termbox.KeyEnd:
			v.navigateEnd()
		case termbox.KeyCtrlH:
			v.dropHighlights()
		case termbox.KeyCtrlS:
			v.focus = &v.info
			v.info.reset(ibModeSave)
			v.info.setInput(v.getFilteredLocationHint())
		}
	}
	return
}

func (v *viewer) getFilteredLocationHint() (hint string) {
	if config.filterOutput == "" {
		hint = v.fetcher.reader.Name() + ".filtered"
	} else {
		hint = path.Join(config.filterOutput, "slit.filtered")
	}
	return
}

func (v *viewer) resize(width, height int) {
	v.sizeLock.Lock()
	v.width, v.height = width, height
	v.height-- // Saving one Line for infobar
	v.sizeLock.Unlock()
	v.info.resize(v.width, v.height)
	v.buffer.window = v.height
	v.draw()
}

type infobarRequest struct {
	str  []rune
	mode infobarMode
}

var requestSearch = make(chan infobarRequest)
var requestRefresh = make(chan struct{})
var requestRefill = make(chan struct{})
var requestStatusUpdate = make(chan LineNo)
var requestKeepCharsChange = make(chan int)

func (v *viewer) termGui() {
	err := termbox.Init()
	if err != nil {
		panic(err)
	}
	defer termbox.Close()

	wg := sync.WaitGroup{}
	ctx, cancel := context.WithCancel(v.ctx)
	defer func() {
		cancel()
		wg.Wait()
	}()

	termbox.SetInputMode(termbox.InputEsc)
	termbox.SetOutputMode(termbox.Output256)
	v.info = infobar{
		y:              0,
		width:          0,
		currentLine:    &v.buffer.originalPos,
		totalLines:     0,
		filtersEnabled: &v.fetcher.filtersEnabled,
		keepChars:      &v.keepChars,
		flock:          &v.fetcher.lock,
		searchType:     filters.CaseSensitive,
	}
	v.focus = v
	v.buffer = viewBuffer{
		fetcher: v.fetcher,
	}
	v.resize(termbox.Size())
	if config.follow {
		v.navigateEnd()
	}
	wg.Add(3)
	go func() { v.refreshIfEmpty(ctx); wg.Done() }()
	go func() { v.updateLastLine(ctx); wg.Done() }()
	go func() { v.follow(ctx); wg.Done() }()
loop:
	for {
		switch ev := termbox.PollEvent(); ev.Type {
		case termbox.EventKey:
			action := v.focus.processKey(ev)
			switch action {
			case ACTION_QUIT:
				break loop
			case ACTION_RESET_FOCUS:
				v.resetFocus()
			}
		case termbox.EventResize:
			logging.Debug("Resize event", ev.Width, ev.Height)
			v.resize(ev.Width, ev.Height)
		case termbox.EventError:
			panic(ev.Err)
		case termbox.EventInterrupt:
			select {
			case search := <-requestSearch:
				v.processInfobarRequest(search)
			case <-requestRefresh:
				v.buffer.refresh()
				v.draw()
			case <-requestRefill: // It is not most efficient solution, it might cause huge amount of redraws
				v.refill()
			case line := <-requestStatusUpdate:
				v.info.totalLines = line + 1
				if v.focus == v {
					v.info.draw()
				}
			case charChange := <-requestKeepCharsChange:
				if v.keepChars+charChange >= 0 {
					v.keepChars = v.keepChars + charChange
				}
				v.draw()
			}
		}
	}

}
func (v *viewer) refill() {
	for {
		result := v.buffer.fill()
		if result.newLines != 0 {
			v.buffer.shift(result.newLines)
			if v.buffer.isFull() {
				v.buffer.shiftToEnd()
			}
			v.draw()
			continue
		}
		if result.lastLineChanged {
			v.draw()
			continue
		}
		return
	}
}

func (v *viewer) saveFiltered(filename string) {
	filename = utils.ExpandHomePath(filename)
	f, err := os.Create(filename)
	if err != nil {
		v.info.setMessage(ibMessage{str: "Err:" + err.Error(), color: termbox.ColorRed})
		logging.Debug(err)
		return
	}
	ctx := context.TODO() // TODO: Use cancel once viewer will be non blocked
	lines := v.fetcher.Get(ctx, Pos{0, 0})
	writer := bufio.NewWriterSize(f, 64*1024)
	v.info.setMessage(ibMessage{str: "Saving...", color: termbox.ColorYellow})
	for l := range lines {
		//TODO: Re-Add colors information
		writer.WriteString(string(l.Str.Runes))
		writer.WriteByte('\n')
	}
	writer.Flush()
	v.info.setMessage(ibMessage{str: fmt.Sprintf("Done! %s", filename), color: termbox.ColorGreen})
	f.Close()
}

func (v *viewer) refreshIfEmpty(ctx context.Context) {
	refresh := func() {
		go termbox.Interrupt()
		select {
		case requestRefresh <- struct{}{}:
		case <-ctx.Done():
			return
		}
	}
	delay := 3 * time.Millisecond
	locked := false

	lock := func() {
		v.buffer.lock.RLock()
		v.sizeLock.Lock()
		locked = true
	}
	unlock := func() {
		if !locked {
			return
		}
		v.buffer.lock.RUnlock()
		v.sizeLock.Unlock()
		locked = false
	}
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-time.After(delay):
			if config.follow {
				break loop
			}
			lock()
			if len(v.buffer.buffer) >= v.height {
				break loop
			}
			if v.buffer.pos != 0 || v.buffer.resetPos.Offset != 0 {
				break loop
			}
			if len(v.fetcher.filters) != 0 && !config.stdin {
				break loop
			}
			unlock()
			if config.stdin && config.isStdinRead() {
				refresh()
				break loop
			}
			delay = time.Duration(utils.Min64(int64(4000*time.Millisecond), int64(delay*2)))
			refresh()
		}
	}
	unlock()
}

func (v *viewer) updateLastLine(ctx context.Context) {
	delay := 10 * time.Millisecond
	lastLine := Pos{0, 0}
	var dataLine PosLine
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-time.After(delay):
			prevLine := lastLine
			dataLine = v.fetcher.advanceLines(lastLine)
			lastLine = dataLine.Pos
			if lastLine != prevLine {
				go termbox.Interrupt()
				select {
				case requestStatusUpdate <- lastLine.Line:
					v.fetcher.updateMap(dataLine)
				case <-ctx.Done():
					return
				}
				delay = 0
			} else if config.stdin && config.isStdinRead() {
				break loop
			} else {
				if delay == 0 {
					delay = 10 * time.Millisecond
				}
				delay = time.Duration(utils.Min64(int64(4000*time.Millisecond), int64(delay*2)))
			}
		}
	}
}

func (v *viewer) follow(ctx context.Context) {
	delay := 100 * time.Millisecond
	lastOffset := v.fetcher.lastOffset()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
			if !config.follow {
				continue
			}
			if v.following {
				prevOffset := lastOffset
				lastOffset = v.fetcher.lastOffset()
				if lastOffset != prevOffset {
					go func() {
						go termbox.Interrupt()
						select {
						case requestRefill <- struct{}{}:
						case <-ctx.Done():
							return
						}
					}()
				}
			}
		}
	}
}

func (v *viewer) processInfobarRequest(search infobarRequest) {
	defer logging.Timeit("Got search request")()
	switch search.mode {
	case ibModeFilter:
		v.addFilter(search.str, filters.FilterIntersect)
	case ibModeAppend:
		v.addFilter(search.str, filters.FilterUnion)
	case ibModeExclude:
		v.addFilter(search.str, filters.FilterExclude)
	case ibModeHighlight:
		v.addFilter(search.str, filters.FilterHighlight)
	case ibModeSave:
		v.saveFiltered(string(search.str))
	case ibModeSearch:
		v.search = search.str
		v.forwardSearch = true
		v.nextSearch(false)
	case ibModeBackSearch:
		v.search = search.str
		v.forwardSearch = false
		v.nextSearch(false)
	case ibModeKeepCharacters:
		keep, err := strconv.Atoi(string(search.str))
		if err != nil || keep < 0 {
			logging.Debug("Err: Keepchar: ", err)
			v.keepChars = 0
		} else {
			v.keepChars = keep
		}
	}
	v.draw()
}
func (v *viewer) navigatePageUp() {
	v.navigate(-v.height)
}
func (v *viewer) navigateHalfPageUp() {
	v.navigate(-v.height / 2)
}
func (v *viewer) navigatePageDown() {
	v.navigate(+v.height)
}
func (v *viewer) navigateHalfPageDown() {
	v.navigate(v.height / 2)
}

func (v *viewer) dropFilters() {
	v.fetcher.lock.Lock()
	newFilters := make([]*filters.Filter, 0)
	for _, filter := range v.fetcher.filters {
		logging.Debug(filter.Action)
		if filter.Action == filters.FilterHighlight {
			newFilters = append(newFilters, filter)
		}
	}
	v.fetcher.filters = newFilters
	v.fetcher.lock.Unlock()
	v.buffer.refresh()
	v.draw()
}

func (v *viewer) dropHighlights() {
	v.fetcher.lock.Lock()
	newFilters := make([]*filters.Filter, 0)
	for _, filter := range v.fetcher.filters {
		if filter.Action != filters.FilterHighlight {
			newFilters = append(newFilters, filter)
		}
	}
	v.fetcher.filters = newFilters
	v.fetcher.highlightedLines = v.fetcher.highlightedLines[:0]
	v.fetcher.lock.Unlock()
	v.buffer.refresh()
	v.draw()
}

func reportSystemUsage() {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	logging.Debug(mem.Alloc)
	logging.Debug("Total alloc", bytefmt.ByteSize(mem.TotalAlloc))
	logging.Debug("Sys", bytefmt.ByteSize(mem.Sys))
	logging.Debug("Heap alloc", bytefmt.ByteSize(mem.HeapAlloc))
	logging.Debug("Heap sys", bytefmt.ByteSize(mem.HeapSys))
	logging.Debug("Goroutines num", runtime.NumGoroutine())
	runtime.GC()
}
