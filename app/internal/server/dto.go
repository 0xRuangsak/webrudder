package server

import "github.com/0xRuangsak/webrudder/internal/browser"

// Request bodies.

type GotoReq struct {
	URL string `json:"url" example:"https://example.com"`
}

type ClickReq struct {
	Ref string `json:"ref" example:"e1"`
}

type FillReq struct {
	Ref  string `json:"ref" example:"e2"`
	Text string `json:"text" example:"user@example.com"`
}

type UploadReq struct {
	Ref  string `json:"ref" example:"e5"`
	File string `json:"file" example:"./avatar.png"`
}

type DownloadReq struct {
	Ref string `json:"ref" example:"e7"`
	Dir string `json:"dir,omitempty"`
}

type BatchReq struct {
	Actions []browser.Action `json:"actions"`
}

// Response bodies (for the OpenAPI schema / Swagger UI).

type StatusResp struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	Port  int    `json:"port"`
}

type ScanResp struct {
	Elements []browser.Element `json:"elements"`
}

type ReadResp struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	Text  string `json:"text"`
}

type ClickResp struct {
	OK         bool   `json:"ok"`
	Navigated  bool   `json:"navigated,omitempty"`
	URL        string `json:"url,omitempty"`
	Downloaded string `json:"downloaded,omitempty"`
	NeedsFile  bool   `json:"needs_file,omitempty"`
}

type GotoResp struct {
	OK  bool   `json:"ok"`
	URL string `json:"url"`
}

type DownloadResp struct {
	OK    bool   `json:"ok"`
	Saved string `json:"saved"`
}

type BatchResp struct {
	OK      bool             `json:"ok"`
	Results []map[string]any `json:"results"`
}

type OKResp struct {
	OK bool `json:"ok"`
}

type ErrResp struct {
	OK    bool   `json:"ok"`
	Error string `json:"error" example:"ref \"e9\" not found — re-scan the page"`
}

type PressReq struct {
	Key string `json:"key" example:"Enter"`
}

type TypeReq struct {
	Text string `json:"text" example:"hello"`
}

type HoverReq struct {
	Ref string `json:"ref" example:"e1"`
}

type ScrollReq struct {
	Ref    string  `json:"ref,omitempty"`
	Dir    string  `json:"dir,omitempty" example:"down"`
	Amount float64 `json:"amount,omitempty"`
}

type SelectReq struct {
	Ref    string   `json:"ref" example:"e3"`
	Values []string `json:"values"`
}

type CheckReq struct {
	Ref     string `json:"ref" example:"e4"`
	Checked bool   `json:"checked" example:"true"`
}

type WaitReq struct {
	Selector string `json:"selector,omitempty"`
	Text     string `json:"text,omitempty"`
	Ms       int    `json:"ms,omitempty"`
	Gone     bool   `json:"gone,omitempty"`
}

type DialogReq struct {
	Accept bool   `json:"accept" example:"true"`
	Text   string `json:"text,omitempty"`
}

// StateDoc documents the /state payload for Swagger. The live handler uses
// browser.State, whose cookies are CDP structs swag can't introspect.
type StateDoc struct {
	Cookies []CookieDoc       `json:"cookies"`
	Local   map[string]string `json:"local"`
	Session map[string]string `json:"session"`
}

type CookieDoc struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain,omitempty"`
	Path   string `json:"path,omitempty"`
}
