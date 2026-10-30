package route

import (
	"bytes"
	rand "math/rand/v2"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ByJotaro/ai-router/internal/provider"
)

// tailSpaces — кодовые точки, которые выглядят как обычный пробел. Мы
// заменяем ими ASCII-пробелы в ХВОСТЕ тела запроса, чтобы одинаковые
// запросы не уходили с разных выходов байт-в-байт (идея: корреляция тел
// между IP — один из сигналов «ферма за прокси»). Все точки — легальные
// whitespace-классы, не содержат `"` и `\`, поэтому JSON остаётся валидным.
var tailSpaces = []rune{
	'\u0020', '\u00a0', '\u2000', '\u2001', '\u2002', '\u2003', '\u2004',
	'\u2005', '\u2006', '\u2007', '\u2008', '\u2009', '\u200a', '\u202f',
	'\u205f', '\u3000',
}

var jitterSeq atomic.Uint64

// jitterSeed — разный на каждый запрос сид (счётчик + время).
func jitterSeed() int64 {
	n := jitterSeq.Add(1)
	return int64(n)*2654435761 + time.Now().UnixNano()
}

// maybeJitterTail применяет tail-jitter, если он включён для провайдера
// (transport.jitter_tail). По умолчанию — no-op: возвращает тот же срез.
func maybeJitterTail(p *provider.Provider, body []byte) []byte {
	if p == nil || !p.Transport.JitterTail || len(body) == 0 {
		return body
	}
	return JitterTailBody(body, jitterSeed())
}

// JitterTailBody переписывает кодовые точки пробелов внутри ПОСЛЕДНЕГО
// строкового значения ключа "content" в теле JSON. Всё, что до него
// (системный промт, история, определения инструментов) остаётся
// байт-в-байт — иначе умрёт prompt-cache апстрима: кэш ключуется по
// префиксу токенов, и мутация префикса делает каждый запрос холодным.
//
// Гарантии: JSON остаётся валидным; текст после нормализации всех
// whitespace-подмен обратно в ' ' совпадает с исходным; при отсутствии
// "content"-строки возвращается исходный срез.
func JitterTailBody(body []byte, seed int64) []byte {
	span := lastContentString(body)
	if span[0] < 0 {
		return body
	}
	start, end := span[0], span[1]
	rng := rand.New(rand.NewPCG(uint64(seed), uint64(seed)^0x9e3779b97f4a7c15))
	out := make([]byte, 0, len(body))
	out = append(out, body[:start]...)
	changed := false
	for i := start; i < end; {
		c := body[i]
		if c == '"' || c == '\\' {
			// Экранированную пару копируем целиком, но мутируем только
			// ASCII-пробел: escape-последовательности не трогаем.
			if c == '\\' && i+1 < end {
				out = append(out, body[i], body[i+1])
				i += 2
				continue
			}
			out = append(out, c)
			i++
			continue
		}
		if c == ' ' && rng.Float64() < 0.5 {
			// Индекс 0 — сам ASCII-пробел: он не меняет байты, поэтому
			// выбираем только homoglyph-варианты.
			r := tailSpaces[1+rng.IntN(len(tailSpaces)-1)]
			out = append(out, string(r)...)
			changed = true
			i++
			continue
		}
		out = append(out, c)
		i++
	}
	out = append(out, body[end:]...)
	if !changed {
		// Ни одна замена не выпала: при включённом флаге тело обязано
		// отличаться, иначе смысл джиттера теряется. Меняем первый ASCII-
		// пробел хвоста на homoglyph (никогда не сам ASCII-пробел).
		tail := out[start:end]
		if i := bytes.IndexByte(tail, ' '); i >= 0 {
			r := tailSpaces[1+rng.IntN(len(tailSpaces)-1)]
			nb := make([]byte, 0, len(out)+3)
			nb = append(nb, out[:start+i]...)
			nb = append(nb, string(r)...)
			nb = append(nb, out[start+i+1:]...)
			return nb
		}
		return body
	}
	return out
}

// lastContentString возвращает [начало, конец) содержимого последнего
// строкового значения после ключа "content", либо [-1,-1].
func lastContentString(body []byte) [2]int {
	key := []byte(`"content"`)
	idx := -1
	for from := 0; ; {
		j := indexFrom(body, key, from)
		if j < 0 {
			break
		}
		idx = j
		from = j + len(key)
	}
	if idx < 0 {
		return [2]int{-1, -1}
	}
	i := idx + len(key)
	for i < len(body) && (body[i] == ' ' || body[i] == '\t' || body[i] == '\n' || body[i] == '\r') {
		i++
	}
	if i >= len(body) || body[i] != ':' {
		return [2]int{-1, -1}
	}
	i++
	for i < len(body) && (body[i] == ' ' || body[i] == '\t' || body[i] == '\n' || body[i] == '\r') {
		i++
	}
	if i >= len(body) || body[i] != '"' {
		return [2]int{-1, -1}
	}
	i++
	start := i
	for i < len(body) {
		switch body[i] {
		case '\\':
			i += 2
			continue
		case '"':
			return [2]int{start, i}
		}
		i++
	}
	return [2]int{-1, -1}
}

func indexFrom(b []byte, sub []byte, from int) int {
	if from >= len(b) {
		return -1
	}
	i := strings.Index(string(b[from:]), string(sub))
	if i < 0 {
		return -1
	}
	return from + i
}
