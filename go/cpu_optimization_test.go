package isureal

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"math/rand"
	"mime/multipart"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

func legacySignature(key []byte, id string, exp int64) []byte {
	value := []byte(strconv.FormatInt(exp, 10) + ":" + id)
	for range imageSignatureIterations {
		mac := hmac.New(sha256.New, key)
		mac.Write(value)
		value = mac.Sum(nil)
	}
	return value
}
func TestSignatureCompatibility(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Go(func() {
			key := bytes.Repeat([]byte{byte(i)}, 32)
			for _, id := range []string{"", "image-id", "画像"} {
				if !bytes.Equal(imageSignature(key, id, int64(i)), legacySignature(key, id, int64(i))) {
					t.Error("signature mismatch")
				}
			}
		})
	}
	wg.Wait()
}
func BenchmarkImageSignature(b *testing.B) {
	key := bytes.Repeat([]byte{1}, 32)
	for _, tc := range []struct {
		name string
		f    func([]byte, string, int64) []byte
	}{{"before", legacySignature}, {"after", imageSignature}} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				tc.f(key, "image-id", 1234567890)
			}
		})
	}
}
func rankingFixture(n int) ([]PostWithAuthor, map[string]int64) {
	posts := make([]PostWithAuthor, n)
	scores := make(map[string]int64, n)
	r := rand.New(rand.NewSource(1))
	for i := range posts {
		posts[i].ID = fmt.Sprintf("%08d", n-i)
		posts[i].PostedAt = time.Unix(int64(n-i), 0)
		scores[posts[i].ID] = int64(r.Intn(20))
	}
	return posts, scores
}
func legacyTop(posts []PostWithAuthor, scores map[string]int64, input getFeedRequest) []PostWithAuthor {
	sort.SliceStable(posts, func(i, j int) bool { return scores[posts[i].ID] > scores[posts[j].ID] })
	out := posts[:0]
	for _, p := range posts {
		if input.BeforeScore != nil && !rankedBefore(scores[p.ID], p.Post, *input.BeforeScore, *input.BeforePostedAt, input.BeforeID) {
			continue
		}
		out = append(out, p)
		if len(out) == 51 {
			break
		}
	}
	return out
}
func TestRankingCompatibility(t *testing.T) {
	for _, n := range []int{0, 1, 50, 51, 100, 101, 1000, 10000} {
		posts, scores := rankingFixture(n)
		for _, cursor := range []bool{false, true} {
			input := getFeedRequest{}
			if cursor {
				score := int64(10)
				at := time.Unix(int64(n/2), 0)
				input.BeforeScore = &score
				input.BeforePostedAt = &at
				input.BeforeID = fmt.Sprintf("%08d", n/2)
			}
			want := legacyTop(append([]PostWithAuthor{}, posts...), scores, input)
			got := topRankedPosts(append([]PostWithAuthor{}, posts...), scores, input)
			// Small sets use the original stable sort and the response applies the cursor.
			if n <= 100 {
				got = legacyTop(got, scores, input)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("ranking mismatch n=%d cursor=%v", n, cursor)
			}
		}
	}
}
func BenchmarkFeedRanking(b *testing.B) {
	for _, n := range []int{50, 1000, 10000} {
		posts, scores := rankingFixture(n)
		work := make([]PostWithAuthor, n)
		for _, before := range []bool{true, false} {
			b.Run(fmt.Sprintf("n=%d/before=%v", n, before), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					copy(work, posts)
					if before {
						legacyTop(work, scores, getFeedRequest{})
					} else {
						topRankedPosts(work, scores, getFeedRequest{})
					}
				}
			})
		}
	}
}
func TestSearchNormalizationCompatibility(t *testing.T) {
	for _, s := range []string{"ABC", "Straße", "İstanbul", "e\u0301", "É", "Σςσ", "hello world", ""} {
		want := norm.NFC.String(cases.Fold().String(norm.NFC.String(s)))
		if got := normalizeSearchText(s); got != want {
			t.Fatalf("%q: %q != %q", s, got, want)
		}
	}
}
func BenchmarkSearchNormalization(b *testing.B) {
	for _, s := range []string{"ExampleUser123", "日本語É"} {
		for _, before := range []bool{true, false} {
			b.Run(fmt.Sprintf("%s/before=%v", s, before), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if before {
						norm.NFC.String(cases.Fold().String(norm.NFC.String(s)))
					} else {
						normalizeSearchText(s)
					}
				}
			})
		}
	}
}
func jpegFixture(b testing.TB) []byte {
	b.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 512, 512)), nil); err != nil {
		b.Fatal(err)
	}
	return buf.Bytes()
}
func TestJPEGNeedsFullDecode(t *testing.T) {
	data := jpegFixture(t)
	data = data[:len(data)-10]
	if _, err := jpeg.DecodeConfig(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(data)); err == nil {
		t.Fatal("truncated JPEG accepted")
	}
}
func BenchmarkJPEGDecode(b *testing.B) {
	data := jpegFixture(b)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
			b.Fatal(err)
		}
	}
}
func BenchmarkImagePartRead(b *testing.B) {
	data := strings.Repeat("jpeg", 256*1024)
	for _, before := range []bool{true, false} {
		b.Run(fmt.Sprintf("before=%v", before), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				f := struct{ *strings.Reader }{strings.NewReader(data)}
				if before {
					io.ReadAll(f)
				} else {
					readImagePart(structFile{f.Reader}, &multipart.FileHeader{Size: int64(len(data))})
				}
			}
		})
	}
}

type structFile struct{ *strings.Reader }

func (structFile) Close() error { return nil }

func TestIPBanMapImmutability(t *testing.T) {
	until := time.Now().Add(time.Hour)
	previous := &ipBanSnapshot{bans: map[string]time.Time{"ip": until}}
	unchanged := ipBanMap([]ipBan{{IP: "ip", BannedUntil: until}}, previous)
	if !unchanged["ip"].Equal(until) {
		t.Fatal("unchanged ban lost")
	}
	changed := ipBanMap([]ipBan{{IP: "ip", BannedUntil: until.Add(time.Hour)}}, previous)
	if !changed["ip"].Equal(until.Add(time.Hour)) || !previous.bans["ip"].Equal(until) {
		t.Fatal("snapshot mutated")
	}
	removed := ipBanMap(nil, previous)
	if len(removed) != 0 || len(previous.bans) != 1 {
		t.Fatal("removal mutated snapshot")
	}
	added := ipBanMap([]ipBan{{IP: "new", BannedUntil: until}}, previous)
	if _, ok := added["ip"]; ok {
		t.Fatal("old ban retained")
	}
}
func BenchmarkIPBanMap(b *testing.B) {
	rows := make([]ipBan, 1000)
	for i := range rows {
		rows[i] = ipBan{IP: strconv.Itoa(i), BannedUntil: time.Unix(2000000000, 0)}
	}
	previous := &ipBanSnapshot{bans: ipBanMap(rows, nil)}
	for _, before := range []bool{true, false} {
		b.Run(fmt.Sprintf("before=%v", before), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if before {
					ipBanMap(rows, nil)
				} else {
					ipBanMap(rows, previous)
				}
			}
		})
	}
}
