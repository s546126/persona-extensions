package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var (
	errToken       = errors.New("monday rejected the personal API token; replace it in extension settings")
	errUnavailable = errors.New("monday could not be reached; try again later")
	errNotFound    = errors.New("monday could not find that board or item")
	errRateLimited = errors.New("monday rate limit reached; try again later")
)

type client struct {
	base, token string
	http        *http.Client
}

func newClient(base, token string) *client {
	return &client{base: strings.TrimRight(base, "/"), token: strings.TrimSpace(token), http: &http.Client{
		Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (c *client) query(ctx context.Context, query string, out any) error {
	if c.token == "" {
		return errors.New("monday personal API token is not configured")
	}
	data, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", errUnavailable, err)
	}
	defer res.Body.Close()
	data, err = io.ReadAll(io.LimitReader(res.Body, 4<<20+1))
	if err != nil {
		return errUnavailable
	}
	if len(data) > 4<<20 {
		return errors.New("monday returned an oversized response")
	}
	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return errToken
	case http.StatusNotFound:
		return errNotFound
	case http.StatusTooManyRequests:
		return errRateLimited
	default:
		return fmt.Errorf("monday returned HTTP %d", res.StatusCode)
	}
	var envelope graphqlResponse
	if err := json.Unmarshal(data, &envelope); err != nil {
		return errors.New("monday returned invalid GraphQL JSON")
	}
	if len(envelope.Errors) > 0 {
		return errors.New("monday GraphQL rejected the operation: " + clipText(envelope.Errors[0].Message, 300))
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return errors.New("monday returned no GraphQL data")
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return errors.New("monday returned an unexpected GraphQL shape")
	}
	return nil
}

func (c *client) me(ctx context.Context) (user, error) {
	var out mePage
	err := c.query(ctx, `query { me { id name } }`, &out)
	return out.Me, err
}

// boardsPageSize is the number of boards asked for per page. The boards query returns 25 when `limit` is left out, and
// `page` starts at 1.
const boardsPageSize = 50

// maxBoards keeps the board list within the SDK's component limit.
const maxBoards = 250

// maxBoardPages caps paginated board reads so repeating pages cannot loop forever.
const maxBoardPages = 20

func (c *client) boards(ctx context.Context) ([]board, error) {
	var boards []board
	seen := map[int64]bool{}
	for page := 1; page <= maxBoardPages && len(boards) < maxBoards; page++ {
		var out boardPage
		if err := c.query(ctx, fmt.Sprintf(`query { boards(limit: %d, page: %d) { id name url } }`, boardsPageSize, page), &out); err != nil {
			return nil, err
		}
		added := 0
		for _, item := range out.Boards {
			if item.ID > 0 && !seen[item.ID] {
				seen[item.ID] = true
				boards = append(boards, item)
				added++
			}
		}
		if added == 0 || len(out.Boards) < boardsPageSize {
			break
		}
	}
	return boards[:min(len(boards), maxBoards)], nil
}

func (c *client) board(ctx context.Context, id int64) (board, error) {
	if id <= 0 {
		return board{}, errors.New("board ID must be positive")
	}
	var out boardPage
	if err := c.query(ctx, fmt.Sprintf(`query { boards(ids: [%d]) { id name url columns { id title type settings_str } } }`, id), &out); err != nil {
		return board{}, err
	}
	if len(out.Boards) != 1 {
		return board{}, errNotFound
	}
	return out.Boards[0], nil
}

func (c *client) items(ctx context.Context, boardID int64, cursor string) (itemsPage, error) {
	if boardID <= 0 {
		return itemsPage{}, errors.New("board ID must be positive")
	}
	if len(cursor) > 2048 || strings.ContainsAny(cursor, "\x00\r\n\"'") {
		return itemsPage{}, errors.New("invalid items_page cursor")
	}
	query := fmt.Sprintf(`query { boards(ids: [%d]) { items_page(limit: 50%s) { cursor items { id name url created_at updated_at column_values { id text value } } } }`, boardID, cursorArgument(cursor))
	var out itemPage
	err := c.query(ctx, query, &out)
	if err != nil {
		return itemsPage{}, err
	}
	if len(out.Boards) != 1 {
		return itemsPage{}, errNotFound
	}
	return out.Boards[0].ItemsPage, nil
}

// item reads one item by its ID, so an item on any page of the board opens. The query is not scoped to a board, so an ID
// that belongs to another board is refused.
func (c *client) item(ctx context.Context, boardID, itemID int64) (item, error) {
	if boardID <= 0 || itemID <= 0 {
		return item{}, errors.New("board and item IDs must be positive")
	}
	query := fmt.Sprintf(`query { items(ids: [%d]) { id name url created_at updated_at board { id } column_values { id text value } } }`, itemID)
	var out itemByIDPage
	if err := c.query(ctx, query, &out); err != nil {
		return item{}, err
	}
	if len(out.Items) != 1 || out.Items[0].Board.ID != boardID {
		return item{}, errNotFound
	}
	return out.Items[0].item, nil
}

func cursorArgument(cursor string) string {
	if cursor == "" {
		return ""
	}
	data, _ := json.Marshal(cursor)
	return ", cursor: " + string(data)
}

func (c *client) createItem(ctx context.Context, boardID int64, name string) (item, error) {
	if boardID <= 0 {
		return item{}, errors.New("board ID must be positive")
	}
	nameJSON, _ := json.Marshal(name)
	query := fmt.Sprintf(`mutation { create_item(board_id: %d, item_name: %s) { id name url } }`, boardID, nameJSON)
	var out createPage
	err := c.query(ctx, query, &out)
	return out.CreateItem, err
}

func (c *client) updateStatusColumn(ctx context.Context, boardID, itemID int64, columnID, label string) (item, error) {
	if boardID <= 0 || itemID <= 0 {
		return item{}, errors.New("board and item IDs must be positive")
	}
	valueObject, _ := json.Marshal(map[string]map[string]string{columnID: {"label": label}})
	valueString, _ := json.Marshal(string(valueObject))
	query := fmt.Sprintf(`mutation { change_multiple_column_values(board_id: %d, item_id: %d, column_values: %s) { id name url column_values { id text value } } }`, boardID, itemID, valueString)
	var out updatePage
	err := c.query(ctx, query, &out)
	return out.ChangeMultipleColumnValues, err
}

func safeBoardLink(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || (u.Hostname() != "monday.com" && !strings.HasSuffix(u.Hostname(), ".monday.com")) || u.User != nil {
		return ""
	}
	return raw
}

func clipText(text string, limit int) string {
	r := []rune(text)
	if len(r) > limit {
		return string(r[:max(0, limit-1)]) + "…"
	}
	return text
}

func pageLimit() string { return strconv.Itoa(50) }
