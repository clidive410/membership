package web

import (
	"html/template"
	"net/url"
	"strconv"
)

// sortCol describes a sortable column: its sort key, header label and
// extra cell classes (e.g. "text-end" for numbers and dates).
type sortCol struct {
	Key, Label, Class string
}

// sortHeader is one sortable table column header.
type sortHeader struct {
	Label    string
	Class    string
	Href     template.URL
	AriaSort string // "ascending", "descending" or "" for unsorted
}

// sortHeaders builds header links that keep the current query (filters) and
// toggle the direction of the active column, like the legacy list pages.
// Changing the sort goes back to the first page.
func sortHeaders(path string, q url.Values, sort string, desc bool, cols []sortCol) []sortHeader {
	out := make([]sortHeader, 0, len(cols))
	for _, c := range cols {
		v := url.Values{}
		for k, vs := range q {
			if k != "sort" && k != "dir" && k != "flash" && k != "n" && k != "page" {
				v[k] = vs
			}
		}
		v.Set("sort", c.Key)
		h := sortHeader{Label: c.Label, Class: c.Class}
		if c.Key == sort {
			if desc {
				h.AriaSort = "descending"
			} else {
				h.AriaSort = "ascending"
				v.Set("dir", "desc")
			}
		}
		h.Href = template.URL(path + "?" + v.Encode())
		out = append(out, h)
	}
	return out
}

// pageSize is the number of members per page in the member list.
const pageSize = 50

type pageLink struct {
	N       int
	Href    template.URL
	Current bool
	Gap     bool // an ellipsis instead of a link
}

// pager describes the pagination of a list.
type pager struct {
	Page, Pages     int
	Total, From, To int
	Prev, Next      template.URL // empty on the first/last page
	Links           []pageLink
}

// paginate clamps the requested page and returns the slice bounds and the
// pager. Links keep the current query (filters and sort).
func paginate(path string, q url.Values, requested, total, size int) (lo, hi int, p pager) {
	pages := max(1, (total+size-1)/size)
	page := min(max(1, requested), pages)
	lo, hi = (page-1)*size, min(page*size, total)
	href := func(n int) template.URL {
		v := url.Values{}
		for k, vs := range q {
			if k != "page" && k != "flash" && k != "n" {
				v[k] = vs
			}
		}
		if n > 1 {
			v.Set("page", strconv.Itoa(n))
		}
		if len(v) == 0 {
			return template.URL(path)
		}
		return template.URL(path + "?" + v.Encode())
	}
	p = pager{Page: page, Pages: pages, Total: total, From: lo + 1, To: hi}
	if total == 0 {
		p.From = 0
	}
	if page > 1 {
		p.Prev = href(page - 1)
	}
	if page < pages {
		p.Next = href(page + 1)
	}
	gap := false
	for n := 1; n <= pages; n++ {
		if n == 1 || n == pages || (n >= page-2 && n <= page+2) {
			p.Links = append(p.Links, pageLink{N: n, Href: href(n), Current: n == page})
			gap = false
		} else if !gap {
			p.Links = append(p.Links, pageLink{Gap: true})
			gap = true
		}
	}
	return lo, hi, p
}
