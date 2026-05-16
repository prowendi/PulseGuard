package domain

import "time"

type ParseMode string

const (
	ParseMarkdownV2 ParseMode = "MarkdownV2"
	ParseHTML       ParseMode = "HTML"
	ParseNone       ParseMode = "None"
)

type Template struct {
	ID        int64
	TenantID  int64
	Name      string
	ParseMode ParseMode
	Body      string
	CreatedAt time.Time
	UpdatedAt time.Time
}
