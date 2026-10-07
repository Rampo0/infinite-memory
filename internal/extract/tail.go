package extract

import (
	"bufio"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

func ReadTail(path string, maxBytes int64) ([]Turn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := max(fi.Size()-maxBytes, 0)
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	r := bufio.NewReaderSize(f, 256*1024)
	if start > 0 {
		if _, err := r.ReadBytes('\n'); err != nil {
			return nil, nil
		}
	}
	var turns []Turn
	lp := newLineParser()
	for {
		line, err := r.ReadBytes('\n')
		if t, ok := lp.parse(line); ok {
			turns = append(turns, t)
		}
		if err != nil {
			return turns, nil
		}
	}
}

const finalReplyTail = 512 * 1024

func HasFinalReply(path, reply string) bool {
	want := squash(reply)
	if want == "" {
		return true
	}
	turns, err := ReadTail(path, finalReplyTail)
	if err != nil {
		return false
	}
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == "assistant" && turns[i].Text != "" {
			return samePrefix(squash(turns[i].Text), want, 200)
		}
	}
	return false
}

func squash(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func samePrefix(a, b string, n int) bool {
	return clipRunes(a, n) == clipRunes(b, n)
}

func LastExchange(turns []Turn, maxChars int) string {
	var user, assistant string
	for i := len(turns) - 1; i >= 0 && (user == "" || assistant == ""); i-- {
		t := turns[i]
		if t.Role == "user" && user == "" {
			user = t.Text
		}
		if t.Role == "assistant" && assistant == "" {
			assistant = t.Text
		}
	}
	return clipRunes(strings.TrimSpace(user+"\n"+assistant), maxChars)
}

func clipRunes(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
