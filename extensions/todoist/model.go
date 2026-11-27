package main

import "time"

// Task IDs and project IDs are opaque strings in Todoist's current API, not numbers.
type task struct {
	ID          string    `json:"id"`
	Content     string    `json:"content"`
	Description string    `json:"description"`
	ProjectID   string    `json:"project_id"`
	SectionID   string    `json:"section_id"`
	ParentID    string    `json:"parent_id"`
	Labels      []string  `json:"labels"`
	Priority    int       `json:"priority"`
	Due         *dueDate  `json:"due"`
	Duration    *duration `json:"duration"`
	AddedAt     string    `json:"added_at"`
	CompletedAt string    `json:"completed_at"`
	ChildOrder  int       `json:"child_order"`
}

// dueDate is the API v1 due object. Date is a full day ("2016-12-01"), a floating wall time in the
// user's zone ("2016-12-01T12:00:00[.ffffff]") or a fixed time stored in UTC ("2016-12-06T13:00:00[.ffffff]Z").
type dueDate struct {
	Date        string `json:"date"`
	String      string `json:"string"`
	IsRecurring bool   `json:"is_recurring"`
	Timezone    string `json:"timezone"`
}

// localDay returns the due day (YYYY-MM-DD) and, for timed dues, the wall clock (HH:MM) in loc.
// Both are empty when there is no due date or Todoist sent a form this version does not know.
func (d *dueDate) localDay(loc *time.Location) (day, clock string) {
	if d == nil {
		return "", ""
	}
	if t, err := time.Parse(time.DateOnly, d.Date); err == nil {
		return t.Format(time.DateOnly), ""
	}
	if t, err := time.Parse(time.RFC3339Nano, d.Date); err == nil {
		t = t.In(loc)
		return t.Format(time.DateOnly), t.Format("15:04")
	}
	if t, err := time.Parse("2006-01-02T15:04:05.999999999", d.Date); err == nil {
		return t.Format(time.DateOnly), t.Format("15:04")
	}
	return "", ""
}

// label is the due day and clock in loc for display; an unknown form is shown as Todoist sent it.
func (d *dueDate) label(loc *time.Location) string {
	day, clock := d.localDay(loc)
	if day == "" {
		return d.Date
	}
	if clock != "" {
		return day + " " + clock
	}
	return day
}

type duration struct {
	Amount int    `json:"amount"`
	Unit   string `json:"unit"`
}

type project struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	ParentID string `json:"parent_id"`
	Inbox    bool   `json:"inbox_project"`
}

type section struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
}

type label struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type contextData struct {
	Projects map[string]project
	Sections map[string]section
	Labels   map[string]label
}

// taskPatch distinguishes omitted (leave unchanged) from explicitly supplied fields.
// The zero value changes nothing; validatePatch requires at least one supplied field.
type taskPatch struct {
	Content     *string   `json:"content,omitempty"`
	Description *string   `json:"description,omitempty"`
	Priority    *int      `json:"priority,omitempty"`
	DueString   *string   `json:"due_string,omitempty"`
	DueDate     *string   `json:"due_date,omitempty"`
	DueDatetime *string   `json:"due_datetime,omitempty"`
	Labels      *[]string `json:"labels,omitempty"`
	ProjectID   *string   `json:"project_id,omitempty"`
	SectionID   *string   `json:"section_id,omitempty"`
}

func (p taskPatch) empty() bool {
	return p.Content == nil && p.Description == nil && p.Priority == nil && p.DueString == nil && p.DueDate == nil &&
		p.DueDatetime == nil && p.Labels == nil && p.ProjectID == nil && p.SectionID == nil
}
