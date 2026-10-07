package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Hardcover is a direct client for Hardcover's GraphQL API. It gives richer
// metadata (description, series, genres, rating, covers) than OpenLibrary and
// needs only a personal API token, configured in the admin panel. When no
// token is set the app falls back to OpenLibrary.
type Hardcover struct {
	store  *Store
	client *http.Client
	log    *log.Logger
}

func NewHardcover(store *Store, logf *log.Logger) *Hardcover {
	return &Hardcover{store: store, client: &http.Client{Timeout: 30 * time.Second}, log: logf}
}

const cfgHardcoverToken = "hardcover_token"

func (h *Hardcover) token() string { return h.store.config(cfgHardcoverToken, "", "") }
func (h *Hardcover) Configured() bool {
	return strings.TrimSpace(h.token()) != ""
}

func (h *Hardcover) gql(ctx context.Context, query string) (json.RawMessage, error) {
	if !h.Configured() {
		return nil, fmt.Errorf("hardcover token not set")
	}
	payload, _ := json.Marshal(map[string]string{"query": query})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.hardcover.app/v1/graphql", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	tok := h.token()
	if !strings.HasPrefix(strings.ToLower(tok), "bearer ") {
		tok = "Bearer " + tok
	}
	req.Header.Set("Authorization", tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hardcover %d: %s", resp.StatusCode, string(body[:min(160, len(body))]))
	}
	var out struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	if len(out.Errors) > 0 {
		return nil, fmt.Errorf("hardcover: %s", out.Errors[0].Message)
	}
	return out.Data, nil
}

// Search returns normalized results via Hardcover's search.
func (h *Hardcover) Search(ctx context.Context, query string) ([]MetaResult, error) {
	q := fmt.Sprintf(`{ search(query: %s, query_type: "Book", per_page: 25){ results } }`, strconv.Quote(query))
	data, err := h.gql(ctx, q)
	if err != nil {
		return nil, err
	}
	var wrap struct {
		Search struct {
			Results struct {
				Hits []struct {
					Document hcDoc `json:"document"`
				} `json:"hits"`
			} `json:"results"`
		} `json:"search"`
	}
	if err := json.Unmarshal(data, &wrap); err != nil {
		return nil, err
	}
	res := make([]MetaResult, 0, len(wrap.Search.Results.Hits))
	for _, hit := range wrap.Search.Results.Hits {
		d := hit.Document
		author := ""
		if len(d.AuthorNames) > 0 {
			author = d.AuthorNames[0]
		}
		var cover *string
		if d.Image != nil && d.Image.URL != "" {
			u := d.Image.URL
			cover = &u
		}
		res = append(res, MetaResult{
			Provider: "hardcover",
			BookID:   string(d.ID),
			Title:    d.Title,
			Author:   author,
			ISBN:     d.firstISBN(),
			CoverURL: cover,
		})
	}
	return res, nil
}

type hcDoc struct {
	ID          hcID     `json:"id"`
	Title       string   `json:"title"`
	Subtitle    *string  `json:"subtitle"`
	AuthorNames []string `json:"author_names"`
	Description string   `json:"description"`
	ReleaseYear *int     `json:"release_year"`
	Pages       *int     `json:"pages"`
	SeriesNames []string `json:"series_names"`
	Genres      []string `json:"genres"`
	Rating      *float64 `json:"rating"`
	Slug        string   `json:"slug"`
	Image       *struct {
		URL string `json:"url"`
	} `json:"image"`
	ISBNs []string `json:"isbns"`
}

// hcID accepts Hardcover's string-or-number ids.
type hcID string

func (h *hcID) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	*h = hcID(s)
	return nil
}

func (d hcDoc) firstISBN() string {
	if len(d.ISBNs) > 0 {
		return d.ISBNs[0]
	}
	return ""
}

// ShelfItem is one entry on a user's Hardcover shelf.
type ShelfItem struct {
	RemoteID    string `json:"remote_id"`
	Title       string `json:"title"`
	Author      string `json:"author"`
	Description string `json:"description"`
	CoverURL    string `json:"cover_url"`
	Year        *int   `json:"year"`
	StatusID    int    `json:"status_id"`
}

// MyShelf pulls the token owner's Hardcover library.
func (h *Hardcover) MyShelf(ctx context.Context) ([]ShelfItem, error) {
	// status_id: 1=Want to Read, 2=Currently Reading, 3=Read, 5=Did Not Finish, 6=Ignored
	q := `{ me { user_books(limit:1000){ status_id book{ id title description release_year image{url} contributions{ author{ name } } } } } }`
	data, err := h.gql(ctx, q)
	if err != nil {
		return nil, err
	}
	var wrap struct {
		Me []struct {
			UserBooks []struct {
				StatusID int `json:"status_id"`
				Book     struct {
					ID          hcID   `json:"id"`
					Title       string `json:"title"`
					Description string `json:"description"`
					ReleaseYear *int   `json:"release_year"`
					Image       *struct {
						URL string `json:"url"`
					} `json:"image"`
					Contributions []struct {
						Author struct {
							Name string `json:"name"`
						} `json:"author"`
					} `json:"contributions"`
				} `json:"book"`
			} `json:"user_books"`
		} `json:"me"`
	}
	if err := json.Unmarshal(data, &wrap); err != nil {
		return nil, err
	}
	if len(wrap.Me) == 0 {
		return nil, nil
	}
	out := make([]ShelfItem, 0, len(wrap.Me[0].UserBooks))
	for _, ub := range wrap.Me[0].UserBooks {
		b := ub.Book
		author := ""
		if len(b.Contributions) > 0 {
			author = strings.TrimSpace(b.Contributions[0].Author.Name)
		}
		item := ShelfItem{
			RemoteID:    string(b.ID),
			Title:       b.Title,
			Author:      author,
			Description: b.Description,
			Year:        b.ReleaseYear,
			StatusID:    ub.StatusID,
		}
		if b.Image != nil {
			item.CoverURL = b.Image.URL
		}
		out = append(out, item)
	}
	return out, nil
}

// Detail fetches one book's full metadata by id.
func (h *Hardcover) Detail(ctx context.Context, id string) (*MetaDetail, error) {
	q := fmt.Sprintf(`{ books(where:{id:{_eq:%s}}){ id title description release_year pages genres } }`, id)
	data, err := h.gql(ctx, q)
	if err != nil {
		return nil, err
	}
	var wrap struct {
		Books []hcDoc `json:"books"`
	}
	if err := json.Unmarshal(data, &wrap); err != nil {
		return nil, err
	}
	if len(wrap.Books) == 0 {
		return nil, nil
	}
	d := wrap.Books[0]
	md := &MetaDetail{
		Title:       d.Title,
		Description: d.Description,
		Genres:      d.Genres,
		PublishYear: d.ReleaseYear,
	}
	if len(d.SeriesNames) > 0 {
		md.SeriesName = &d.SeriesNames[0]
	}
	if d.Image != nil && d.Image.URL != "" {
		u := d.Image.URL
		md.CoverURL = &u
	}
	_ = d.Slug
	return md, nil
}
