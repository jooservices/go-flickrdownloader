package ui

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	ColorReset  = "\033[0m"
	ColorBold   = "\033[1m"
	ColorDim    = "\033[2m"
	ColorRed    = "\033[31m"
	ColorGreen  = "\033[32m"
	ColorYellow = "\033[33m"
	ColorCyan   = "\033[36m"

	IconOk    = "✓"
	IconSkip  = "○"
	IconLink  = "⇄"
	IconErr   = "✗"
	IconPhoto = "📷"
	IconClock = "⏱"
	IconDisk  = "💾"
	IconSpark = "✨"
)

const (
	barWidth = 40
)

type FailEntry struct {
	ID  string
	URL string
	Err string
}

type Stats struct {
	Total    int64
	Success  int64
	Skipped  int64
	Linked   int64
	Failed   int64
	Bytes    int64
	Failures []FailEntry
	mu       sync.Mutex
}

type Progress struct {
	stats *Stats
	start time.Time
}

func NewProgress(total int) *Progress {
	return &Progress{
		stats: &Stats{Total: int64(total)},
		start: time.Now(),
	}
}

func (p *Progress) AddSuccess()      { atomic.AddInt64(&p.stats.Success, 1) }
func (p *Progress) AddSkipped()      { atomic.AddInt64(&p.stats.Skipped, 1) }
func (p *Progress) AddLinked()       { atomic.AddInt64(&p.stats.Linked, 1) }
func (p *Progress) AddBytes(n int64) { atomic.AddInt64(&p.stats.Bytes, n) }

func (p *Progress) AddFailure(id, url, errMsg string) {
	atomic.AddInt64(&p.stats.Failed, 1)
	p.stats.mu.Lock()
	p.stats.Failures = append(p.stats.Failures, FailEntry{ID: id, URL: url, Err: errMsg})
	p.stats.mu.Unlock()
}

func (p *Progress) Stats() *Stats { return p.stats }

func (p *Progress) completed() int64 {
	return p.stats.Success + p.stats.Skipped + p.stats.Linked + p.stats.Failed
}

func (p *Progress) elapsed() time.Duration { return time.Since(p.start) }

func (p *Progress) eta() time.Duration {
	done := p.completed()
	if done == 0 {
		return 0
	}
	return time.Duration(float64(p.elapsed()) / float64(done) * float64(p.stats.Total-done))
}

func (p *Progress) speed() float64 {
	elapsed := p.elapsed().Seconds()
	if elapsed < 0.1 {
		return 0
	}
	return float64(atomic.LoadInt64(&p.stats.Bytes)) / elapsed
}

func (p *Progress) percent() float64 {
	if p.stats.Total == 0 {
		return 0
	}
	return float64(p.completed()) / float64(p.stats.Total) * 100
}

func (p *Progress) Render() string {
	var b strings.Builder

	done := p.completed()
	total := p.stats.Total
	pct := p.percent()

	b.WriteString("  ")
	filled := int(float64(barWidth) * pct / 100)
	if filled < 0 {
		filled = 0
	}
	if filled > barWidth {
		filled = barWidth
	}
	empty := barWidth - filled

	b.WriteString(ColorCyan)
	b.WriteString("[")
	b.WriteString(ColorGreen)
	b.WriteString(strings.Repeat("━", filled))
	if filled < barWidth {
		b.WriteString(ColorYellow)
		b.WriteString("▶")
		empty--
	}
	b.WriteString(ColorDim)
	b.WriteString(strings.Repeat("─", max(empty, 0)))
	b.WriteString(ColorCyan)
	b.WriteString("]")
	b.WriteString(ColorReset)

	b.WriteString(fmt.Sprintf(" %s%3.0f%%%s", ColorBold, pct, ColorReset))
	b.WriteString(fmt.Sprintf("  %s%d%s/%s%d%s",
		ColorGreen, done, ColorReset,
		ColorCyan, total, ColorReset,
	))

	eta := p.eta()
	if eta > 0 && done < total {
		b.WriteString(fmt.Sprintf("  %s%s %seta %s%s%s",
			ColorDim, IconClock, ColorReset,
			ColorYellow, formatDuration(eta), ColorReset,
		))
	}

	spd := p.speed()
	if spd > 0 {
		b.WriteString(fmt.Sprintf("  %s%s%s",
			ColorDim, formatSpeed(spd), ColorReset,
		))
	}

	return b.String()
}

func Bordered(text, color string) string {
	width := utf8.RuneCountInString(text) + 4
	top := "╔" + strings.Repeat("═", width) + "╗"
	mid := "║  " + text + "  ║"
	bot := "╚" + strings.Repeat("═", width) + "╝"
	return color + top + "\n" + mid + "\n" + bot + ColorReset
}

func (p *Progress) Summary() string {
	var b strings.Builder

	b.WriteString("\n")
	b.WriteString(Bordered("Download Complete", ColorGreen))
	b.WriteString(fmt.Sprintf("\n  %s%s Elapsed:%s %s\n",
		ColorDim, IconClock, ColorReset, formatDuration(p.elapsed())))

	ok := ColorGreen + fmt.Sprintf("%s %d", IconOk, p.stats.Success) + ColorReset
	skip := ColorYellow + fmt.Sprintf("%s %d", IconSkip, p.stats.Skipped) + ColorReset
	fail := ColorRed + fmt.Sprintf("%s %d", IconErr, p.stats.Failed) + ColorReset
	b.WriteString(fmt.Sprintf("  %s  %s  %s", ok, skip, fail))
	if p.stats.Linked > 0 {
		b.WriteString(fmt.Sprintf("  %s%s %d linked%s",
			ColorCyan, IconLink, p.stats.Linked, ColorReset))
	}

	if p.stats.Bytes > 0 {
		b.WriteString(fmt.Sprintf("  %s%s %s%s",
			ColorDim, IconDisk, formatBytes(p.stats.Bytes), ColorReset,
		))
	}

	p.stats.mu.Lock()
	failures := p.stats.Failures
	p.stats.mu.Unlock()

	if len(failures) > 0 {
		b.WriteString(fmt.Sprintf("\n\n  %s%s Failed photos:%s\n", ColorRed, IconErr, ColorReset))
		for _, f := range failures {
			b.WriteString(fmt.Sprintf("    %s%s %s — %s%s\n",
				ColorDim, f.ID, f.URL, f.Err, ColorReset))
		}
	}

	spd := p.speed()
	if spd > 0 {
		b.WriteString(fmt.Sprintf("  %s%s avg%s", ColorDim, formatSpeed(spd), ColorReset))
	}

	b.WriteString("\n")
	return b.String()
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	return fmt.Sprintf("%dh%02dm", h, m)
}

func formatSpeed(bytesPerSec float64) string {
	return formatBytes(int64(bytesPerSec)) + "/s"
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n := n / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// FormatBytes renders a byte count in a human-friendly form.
func FormatBytes(n int64) string { return formatBytes(n) }
