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
	OK        bool   `json:"ok"`
	Navigated bool   `json:"navigated,omitempty"`
	URL       string `json:"url,omitempty"`
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
