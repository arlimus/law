package main

import (
	"reflect"
	"testing"
)

func TestParseVercelCommentCurrent(t *testing.T) {
	body := `The latest updates on your projects. Learn more about [Vercel for GitHub](https://vercel.link/github-learn-more).

| Project | Deployment | Actions | Updated (UTC) |
| :--- | :----- | :------ | :------ |
| [artemis-components](https://vercel.com/mondoo/artemis-components) | ![Ready](https://vercel.com/static/status/ready.svg) [Ready](https://vercel.com/mondoo/artemis-components/abc) | [Preview](https://artemis-components-git-feat.preview.mondoo.love), [Comment](https://vercel.live/x) | Apr 27, 2026 9:42pm |
| [console-next](https://vercel.com/mondoo/console-next) | ![Ready](https://vercel.com/static/status/ready.svg) [Ready](https://vercel.com/mondoo/console-next/def) | [Preview](https://console-next-git-feat.preview.mondoo.love), [Comment](https://vercel.live/x) | Apr 27, 2026 9:42pm |
`
	want := []vercelPreview{
		{Name: "artemis-components", URL: "https://artemis-components-git-feat.preview.mondoo.love"},
		{Name: "console-next", URL: "https://console-next-git-feat.preview.mondoo.love"},
	}
	if got := parseVercelComment(body); !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %#v\nwant %#v", got, want)
	}
}

func TestParseVercelCommentLegacy(t *testing.T) {
	body := `**The latest updates on your projects.**

| Name | Status | Preview | Comments | Updated (UTC) |
| :--- | :----- | :------ | :------- | :------------ |
| **artemis-components** | ✅ Ready ([Inspect](https://vercel.com/x)) | [Visit Preview](https://artemis-old.preview.test) | 💬 [**Add feedback**](https://vercel.live/x) | Aug 12, 2024 |
`
	want := []vercelPreview{
		{Name: "artemis-components", URL: "https://artemis-old.preview.test"},
	}
	if got := parseVercelComment(body); !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %#v\nwant %#v", got, want)
	}
}

func TestParseVercelCommentNoMatches(t *testing.T) {
	cases := []string{
		"",
		"just some text without any preview links",
		"| Project | Deployment |\n| :--- | :--- |\n| no preview link here | yep |",
	}
	for _, body := range cases {
		if got := parseVercelComment(body); len(got) != 0 {
			t.Errorf("parseVercelComment(%q) = %#v, want empty", body, got)
		}
	}
}

// Picks the project name from the first [name](https://vercel.com/...) on the row,
// not the later [Ready](https://vercel.com/...) deployment-status link.
func TestParseVercelCommentPicksProjectNotReady(t *testing.T) {
	body := `| [my-app](https://vercel.com/team/my-app) | [Ready](https://vercel.com/team/my-app/dep) | [Preview](https://my-app.preview.test) |`
	got := parseVercelComment(body)
	if len(got) != 1 || got[0].Name != "my-app" {
		t.Fatalf("expected name=my-app, got %#v", got)
	}
}
