// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"testing"
	"time"
)

func TestISO(t *testing.T) {
	cases := []struct {
		name string
		in   time.Time
		want string
	}{
		{"whole seconds", time.Date(2026, 1, 2, 20, 8, 5, 0, jst), "2026-01-02T20:08:05+09:00"},
		{"fractional seconds", time.Date(2026, 1, 2, 20, 8, 5, 123456000, jst), "2026-01-02T20:08:05.123456+09:00"},
		{"utc", time.Date(2024, 8, 16, 12, 30, 0, 0, time.UTC), "2024-08-16T12:30:00+00:00"},
	}
	for _, tc := range cases {
		if got := iso(tc.in); got != tc.want {
			t.Errorf("%s: iso() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestTimestampFromURL(t *testing.T) {
	got, ok := timestampFromURL("https://i.pximg.net/img-original/img/2026/01/02/20/08/05/123456789_p0.png")
	if !ok {
		t.Fatal("timestampFromURL() reported no match")
	}
	want := time.Date(2026, 1, 2, 20, 8, 5, 0, jst)
	if !got.Equal(want) {
		t.Errorf("timestampFromURL() = %v, want %v", got, want)
	}
	if _, off := got.Zone(); off != 9*60*60 {
		t.Errorf("timestampFromURL() offset = %d, want JST", off)
	}

	for _, bad := range []string{
		"",
		"https://i.pximg.net/img-original/123456789_p0.png",
		"https://i.pximg.net/img-original/img/2026/13/02/20/08/05/123456789_p0.png",
	} {
		if _, ok := timestampFromURL(bad); ok {
			t.Errorf("timestampFromURL(%q) unexpectedly matched", bad)
		}
	}
}

func TestValidHeader(t *testing.T) {
	cases := []struct {
		ext  string
		data []byte
		want bool
	}{
		{"jpg", []byte{0xff, 0xd8, 0xff, 0xe0}, true},
		{"jpeg", []byte{0xff, 0xd8, 0xff, 0xe1}, true},
		{"png", []byte("\x89PNG\r\n\x1a\nrest"), true},
		{"gif", []byte("GIF89a..."), true},
		{"gif", []byte("GIF87a..."), true},
		{"webp", []byte("RIFF\x00\x00\x00\x00WEBPVP8 "), true},
		{"webp", []byte("RIFF\x00\x00\x00\x00WAVE"), false},
		{"webp", []byte("RIFF"), false},
		{"png", []byte{0xff, 0xd8, 0xff}, false},
		{"jpg", []byte("<html>"), false},
		{"jpg", nil, false},
		{"bmp", []byte("BM"), false},
	}
	for _, tc := range cases {
		if got := validHeader(tc.ext, tc.data); got != tc.want {
			t.Errorf("validHeader(%q, %q) = %v, want %v", tc.ext, tc.data, got, tc.want)
		}
	}
}

func TestSecondsBetween(t *testing.T) {
	start := time.Date(2026, 1, 2, 20, 8, 0, 0, jst)

	got := secondsBetween(start, start.Add(59*time.Second))
	if len(got) != 60 {
		t.Fatalf("len = %d, want 60", len(got))
	}
	for i, ts := range got {
		if want := start.Add(time.Duration(i) * time.Second); !ts.Equal(want) {
			t.Fatalf("got[%d] = %v, want %v", i, ts, want)
		}
	}

	if got := secondsBetween(start, start); len(got) != 1 || !got[0].Equal(start) {
		t.Errorf("same start/end = %v, want [%v]", got, start)
	}
}

func TestCandidateURL(t *testing.T) {
	// A UTC timestamp must be rendered in JST in the CDN path.
	c := Candidate{Timestamp: time.Date(2026, 1, 2, 11, 8, 5, 0, time.UTC), Page: 3, Ext: "png"}
	want := cdnURL + "/img-original/img/2026/01/02/20/08/05/123456789_p3.png"
	if got := c.URL(123456789); got != want {
		t.Errorf("Candidate.URL() = %q, want %q", got, want)
	}

	// Round trip through timestampFromURL.
	ts, ok := timestampFromURL(c.URL(1))
	if !ok || !ts.Equal(c.Timestamp) {
		t.Errorf("round trip = %v, %v; want %v", ts, ok, c.Timestamp)
	}
}
