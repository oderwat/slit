package main

import (
	"github.com/nsf/termbox-go"
	"github.com/tigrawap/slit/ansi"
	"github.com/tigrawap/slit/logging"
	"context"
	"github.com/tigrawap/slit/runes"
	"runtime"
	"time"
	"io"
	"code.cloudfoundry.org/bytefmt"
)

type viewer struct {
	pos           int
	hOffset       int
	width         int
	height        int
	wrap          bool
	fetcher       *fetcher
	focus         Focusing
	info          infobar
	searchMode    infobarMode
	forwardSearch bool
	search        []rune
	buffer        viewBuffer
}

type action uint

const (
	NO_ACTION          action = iota
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
	index := -1
	if index = v.buffer.searchForward(v.search); index != -1 {
		v.navigate(index)
		return
	}
	if index = v.fetcher.Search(context.TODO(), v.buffer.lastLine().Pos, v.search); index != -1 {
		v.buffer.reset(index)
	}
}

func (v *viewer) searchBack() {
	index := -1
	if index = v.buffer.searchBack(v.search); index != -1 {
		v.navigate(-index)
		return
	}
	if index = v.fetcher.SearchBack(context.TODO(), v.buffer.currentLine().Pos, v.search); index != -1 {
		v.buffer.reset(index)
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

var stylesMap = map[uint8]termbox.Attribute{
	1: termbox.AttrBold,
}

//func (v *viewer) getLine(line int) ansi.Astring {
//	ctx, cancel := context.WithCancel(context.Background())
//	defer cancel()
//	dataChan := v.fetcher.Get(ctx, int32(line), true)
//	data := <-dataChan
//	return data.Str
//}

func (v *viewer) draw() {
	termbox.Clear(termbox.ColorDefault, termbox.ColorDefault)
	for ty, dataLine := 0, 0; ty < v.height; ty++ {
		var tx int
		data, err := v.buffer.getLine(dataLine)
		if err == io.EOF{
			break
		}
		var chars []rune
		var attrs []ansi.RuneAttr
		if v.hOffset > len(data.Runes)-1 {
			chars = data.Runes[:0]
			attrs = data.Attrs[:0]
		} else {
			chars = data.Runes[v.hOffset:]
			attrs = data.Attrs[v.hOffset:]
		}
		var hlIndices []int
		hlChars := 0
		if len(v.search) != 0 {
			hlIndices = runes.IndexAll(chars, v.search)
		} else {
			hlIndices = []int{}
		}
		for i, char := range chars {
			attr := attrs[i]
			bg := termbox.ColorBlack
			fg := termbox.ColorWhite
			if len(hlIndices) != 0 && hlChars == 0 {
				if hlIndices[0] == i {
					hlIndices = hlIndices[1:]
					hlChars = len(v.search)
				}
			}
			if hlChars != 0 {
				bg = termbox.ColorYellow
				hlChars--
			}
			if attr.Fg != 0 {
				fg = termbox.Attribute(attr.Fg-30+1) | stylesMap[attr.Style]
			}
			if bg == termbox.ColorBlack { // If already highlighted by search - dont use original color
				if attr.Bg != 0 {
					bg = termbox.Attribute(attr.Bg - 40 + 1)
				}
			}
			termbox.SetCell(tx, ty, char, fg, bg)
			if !v.wrap {
			}
			tx++
			if tx >= v.width {
				if v.wrap {
					tx = 0
					ty++
				} else {
					break
				}
			}
		}
		if ty >= v.height {
			break
		}
		dataLine++
	}
	v.info.draw()
	termbox.Flush()
}

func (v *viewer) navigate(direction int) {
	v.buffer.shift(direction)
	v.draw()
}

func (v *viewer) navigateEnd() {
	v.buffer.reset(v.fetcher.lastLine())
	v.navigate(- v.height + 1)
	v.draw()
}

func (v *viewer) navigateStart() {
	v.buffer.reset(0)
	v.draw()
}

func (v *viewer) navigateRight() {
	v.wrap = false
	v.hOffset += v.width / 2
	v.draw()
}

func (v *viewer) navigateLeft() {
	v.wrap = false
	v.hOffset -= v.width / 2
	if v.hOffset < 0 {
		v.hOffset = 0
	}
	v.draw()
}

func (v *viewer) resetFocus() {
	v.focus = v
	termbox.HideCursor()
	termbox.Flush()
}

func (v *viewer) processKey(ev termbox.Event) (a action) {
	if ev.Ch != 0 {
		switch ev.Ch {
		case 'W':
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
		case 'g':
			v.navigateStart()
		case 'G':
			v.navigateEnd()
		case 'f':
			v.navigate(+v.height)
		case 'b':
			v.navigate(-v.height)
		case '/':
			v.focus = &v.info
			v.info.reset(ibModeSearch)
		case '&':
			v.focus = &v.info
			v.info.reset(ibModeFilter)
		case '?':
			v.focus = &v.info
			v.info.reset(ibModeBackSearch)
		case '+':
			v.focus = &v.info
			v.info.reset(ibModeAppend)
		case '-':
			v.focus = &v.info
			v.info.reset(ibModeExclude)
		case 'M':
			reportSystemUsage()
		case '=':
			v.fetcher.filters = v.fetcher.filters[:0]
			v.buffer.reset(v.buffer.currentLine().Pos)
			v.draw()
		}
	} else {
		switch ev.Key {
		case termbox.KeyArrowDown:
			v.navigate(+1)
		case termbox.KeyArrowRight:
			v.navigateRight()
		case termbox.KeyArrowLeft:
			v.navigateLeft()
		case termbox.KeyArrowUp:
			v.navigate(-1)
		case termbox.KeyPgup:
			v.navigate(-v.height)
		case termbox.KeyPgdn:
			v.navigate(+v.height)
		case termbox.KeyHome:
			v.navigateStart()
		case termbox.KeyEnd:
			v.navigateEnd()
		}
	}
	return
}

func (v *viewer) resize(width, height int) {
	v.width, v.height = width, height
	v.height-- // Saving one line for infobar
	v.info.resize(v.width, v.height)
	v.buffer.window = v.height
	v.draw()
}

type searchRequest struct {
	str  []rune
	mode infobarMode
}

var requestSearch = make(chan searchRequest)

func (v *viewer) termGui() {
	err := termbox.Init()
	if err != nil {
		panic(err)
	}
	defer termbox.Close()
	termbox.SetInputMode(termbox.InputEsc)
	termbox.SetOutputMode(termbox.Output256)
	v.info = infobar{y: 0, width: 0, currentLine:&v.buffer.originalPos, totalLines:&v.fetcher.totalLines}
	v.focus = v
	v.buffer = viewBuffer{
		fetcher: v.fetcher,
	}
	v.resize(termbox.Size())
loop:
	for {
		switch ev := termbox.PollEvent(); ev.Type {
		case termbox.EventKey:
			// Globals
			//logging.Debug(ev.Key, ev.Mod, ev.Ch)
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
				v.processSearch(search)
			case <- time.After(10 * time.Millisecond):
				continue
			}
		}
	}

}
func (v *viewer) processSearch(search searchRequest) {
	logging.Debug("Got search request")
	switch search.mode {
	case ibModeFilter:
		v.fetcher.filters = append(v.fetcher.filters, &includeFilter{search.str, false})
		v.buffer.reset(v.buffer.currentLine().Pos)
	case ibModeAppend:
		v.fetcher.filters = append(v.fetcher.filters, &includeFilter{search.str, true})
		v.buffer.reset(v.buffer.currentLine().Pos)
	case ibModeExclude:
		v.fetcher.filters = append(v.fetcher.filters, &excludeFilter{search.str})
		v.buffer.reset(v.buffer.currentLine().Pos)
	case ibModeSearch:
		v.search = search.str
		v.forwardSearch = true
		v.nextSearch(false)
	case ibModeBackSearch:
		v.search = search.str
		v.forwardSearch = false
		v.nextSearch(false)
	}
	v.draw()
}

func reportSystemUsage(){
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