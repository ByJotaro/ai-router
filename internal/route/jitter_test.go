package route

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ByJotaro/ai-router/internal/provider"
)

// normTail возвращает строку с приведёнными к ASCII-пробелу homoglyph-пробелами.
func normTail(s string) string {
	return strings.Map(func(r rune) rune {
		for _, sp := range tailSpaces {
			if r == sp {
				return ' '
			}
		}
		return r
	}, s)
}

type chatBody struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Stream bool `json:"stream"`
}

const jitterSample = `{"model":"muse-spark-1.2-contributor-free","messages":[{"role":"system","content":"SYS  A  B"},{"role":"user","content":"hello  world  again  and  again"}],"stream":true}`

// Tail-jitter: мутируется только хвост (последний content), префикс и семантика
// текста сохраняются, JSON остаётся валидным, результат непустой и детерминирован.
func TestJitterTailBody_MutatesOnlyTailKeepingSemantics(t *testing.T) {
	body := []byte(jitterSample)
	out := JitterTailBody(body, 7)
	if !json.Valid(out) {
		t.Fatalf("после джиттера тело невалидно: %s", out)
	}
	prefix := []byte(`{"model":"muse-spark-1.2-contributor-free","messages":[{"role":"system","content":"SYS  A  B"},{"role":"user","content":"`)
	if !bytes.HasPrefix(out, prefix) {
		t.Fatalf("префикс тела изменён (умрёт prompt-cache):\n%q", out[:len(prefix)+20])
	}
	if !bytes.HasSuffix(out, []byte(`"}],"stream":true}`)) {
		t.Fatalf("хвост структуры изменён: %q", out[len(out)-30:])
	}
	var a, b chatBody
	if err := json.Unmarshal(body, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &b); err != nil {
		t.Fatal(err)
	}
	if a.Messages[0].Content != b.Messages[0].Content {
		t.Fatalf("system-сообщение изменено: %q != %q", a.Messages[0].Content, b.Messages[0].Content)
	}
	if normTail(a.Messages[1].Content) != normTail(b.Messages[1].Content) {
		t.Fatalf("семантика хвоста изменилась: %q != %q", a.Messages[1].Content, b.Messages[1].Content)
	}
	if out == nil || string(out) == jitterSample {
		t.Fatalf("джиттер не изменил ни одного пробела: %s", out)
	}
	if !strings.ContainsFunc(string(out), func(r rune) bool { return r > 0x7f }) {
		t.Fatalf("в хвосте нет ни одной homoglyph-замены: %q", out)
	}
}

func TestJitterTailBody_DeterministicForSeed(t *testing.T) {
	body := []byte(jitterSample)
	a := JitterTailBody(body, 42)
	b := JitterTailBody(body, 42)
	if !bytes.Equal(a, b) {
		t.Fatalf("одинаковый сид даёт разный результат:\n%q\n%q", a, b)
	}
	c := JitterTailBody(body, 43)
	if bytes.Equal(a, c) {
		t.Log("предупреждение: сиды 42 и 43 дали одинаковый результат (возможно, но маловероятно)")
	}
}

func TestJitterTailBody_NoContentKeyIsNoop(t *testing.T) {
	body := []byte(`{"model":"m","prompt":"no content key here","stream":true}`)
	out := JitterTailBody(body, 1)
	if !bytes.Equal(out, body) {
		t.Fatalf("тело без content изменено: %q", out)
	}
}

// Ключевой предохранитель: без второго ключа tail-jitter выключен.
func TestMaybeJitterTail_DisabledByDefault(t *testing.T) {
	body := []byte(jitterSample)
	p := &provider.Provider{Name: "opencode"}
	if out := maybeJitterTail(p, body); !bytes.Equal(out, body) {
		t.Fatalf("джиттер применился при выключенном флаге")
	}
	p.Transport.JitterTail = true
	if out := maybeJitterTail(p, body); bytes.Equal(out, body) {
		t.Fatalf("джиттер не применился при включённом флаге")
	}
	if out := maybeJitterTail(nil, body); !bytes.Equal(out, body) {
		t.Fatalf("nil-провайдер должен быть no-op")
	}
}

// Ключевой предохранитель: direct-fallback не может уйти в прямой выход без
// явного allow_home_ip_direct (иначе домашний IP снова утечёт в апстрим и
// получит 403 «abusive traffic»).
func TestTryDirectFallback_RequiresSecondKey(t *testing.T) {
	pt := &PoolTransport{DirectFallback: true}
	rt := &poolRetryRT{t: pt}
	if got := rt.tryDirectFallback(nil, []byte("{}"), func(int) bool { return true }, time.Now().Add(time.Minute), 1); got != nil {
		t.Fatalf("прямой выход разрешён без allow_home_ip_direct")
	}
}

// TestMaybeJitterTail_AlwaysDiffersWhenEnabled — инвариант флага: если в
// хвосте есть пробел, включённый jitter обязан изменить тело при любом сиде.
func TestMaybeJitterTail_AlwaysDiffersWhenEnabled(t *testing.T) {
	body := []byte(jitterSample)
	for seed := int64(0); seed < 200; seed++ {
		if out := JitterTailBody(body, seed); bytes.Equal(out, body) {
			t.Fatalf("seed %d: тело не изменилось", seed)
		}
	}
}
