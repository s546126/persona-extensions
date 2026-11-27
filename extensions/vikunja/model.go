package main

import (
	"strings"
	"time"
)

type user struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Username string `json:"username"`
}

type task struct {
	ID          int64     `json:"id"`
	Identifier  string    `json:"identifier"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Done        bool      `json:"done"`
	DueDate     string    `json:"due_date"`
	ProjectID   int64     `json:"project_id"`
	Assignees   []user    `json:"assignees"`
	Comments    []comment `json:"comments"`
	Updated     string    `json:"updated"`
}

type comment struct {
	ID      int64  `json:"id"`
	Comment string `json:"comment"`
	Created string `json:"created"`
	Author  user   `json:"author"`
}

type project struct {
	ID         int64  `json:"id"`
	Title      string `json:"title"`
	Identifier string `json:"identifier"`
	IsArchived bool   `json:"is_archived"`
	DeletedAt  string `json:"deleted_at"`
}

type taskPage struct {
	Items     []task
	HasMore   bool
	NextPage  int
	Total     int
	PageCount int
}

// due is the task's due date. Vikunja sends the zero time 0001-01-01T00:00:00Z, not null, for a task that has none;
// that reads as "".
func (t task) due() string {
	if parsed, err := time.Parse(time.RFC3339, t.DueDate); err == nil && parsed.Year() <= 1 {
		return ""
	}
	return t.DueDate
}

// dueLabel is the due date for the tools, "none" when the task has none.
func (t task) dueLabel() string {
	if due := t.due(); due != "" {
		return due
	}
	return "none"
}

// subtitle is the identifier, followed by the due date when the task has one.
func (t task) subtitle() string {
	id := strings.TrimSpace(t.Identifier)
	due := strings.TrimSpace(t.due())
	switch {
	case id != "" && due != "":
		return id + " · " + due
	case id != "":
		return id
	default:
		return due
	}
}
