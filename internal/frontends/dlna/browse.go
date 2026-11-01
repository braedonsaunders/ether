package dlna

import (
	"encoding/xml"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func objectFor(title frontend.Title, parent string) didlObject {
	class := "object.item.videoItem.movie"
	name := "item"
	if string(title.Type) == "show" || string(title.Type) == "season" {
		class = "object.container.storageFolder"
		name = "container"
	}
	return didlObject{XMLName: xml.Name{Local: name}, ID: "item:" + strconv.FormatInt(int64(title.Key), 10), ParentID: parent, Restricted: "1", Title: title.Title, Class: class}
}
func viewObject(view frontend.View) didlObject {
	return didlObject{XMLName: xml.Name{Local: "container"}, ID: "view:" + view.ID, ParentID: "0", Restricted: "1", Title: view.Name, Class: "object.container.storageFolder"}
}

func (i *instance) browse(r *http.Request, viewer *frontend.Viewer, device frontend.Device, actionName, id string, args browseArgs) ([]didlObject, int, error) {
	if viewer == nil || i.host.Experience == nil {
		return nil, 0, errors.New("unavailable")
	}
	if id == "" {
		id = "0"
	}
	if actionName == "Browse" && args.BrowseFlag == "BrowseMetadata" {
		if id == "0" {
			return []didlObject{{XMLName: xml.Name{Local: "container"}, ID: "0", ParentID: "-1", Restricted: "1", Title: i.serverName(), Class: "object.container.storageFolder"}}, 1, nil
		}
		if strings.HasPrefix(id, "view:") {
			views, err := i.host.Experience.Views(r.Context(), viewer)
			if err != nil {
				return nil, 0, err
			}
			for _, view := range views {
				if "view:"+view.ID == id {
					return []didlObject{viewObject(view)}, 1, nil
				}
			}
			return nil, 0, errors.New("not found")
		}
		if key, err := objectKey(id); err == nil {
			detail, err := i.host.Experience.Item(r.Context(), frontend.ItemRequest{Viewer: viewer, Device: device, Key: key})
			if err != nil {
				return nil, 0, err
			}
			return []didlObject{objectFor(detail.Title, "0")}, 1, nil
		}
		return nil, 0, errors.New("not found")
	}
	if actionName == "Browse" && args.BrowseFlag != "BrowseDirectChildren" {
		return nil, 0, errors.New("invalid browse flag")
	}
	if id == "0" && actionName == "Browse" {
		views, err := i.host.Experience.Views(r.Context(), viewer)
		if err != nil {
			return nil, 0, err
		}
		total := len(views)
		start := min(args.StartingIndex, total)
		end := min(start+args.RequestedCount, total)
		out := make([]didlObject, 0, end-start)
		for _, view := range views[start:end] {
			out = append(out, viewObject(view))
		}
		return out, total, nil
	}
	query := frontend.ItemQuery{Offset: args.StartingIndex, Limit: args.RequestedCount}
	var page frontend.Page
	var err error
	if actionName == "Search" {
		text, ok := searchText(args.SearchCriteria)
		if !ok {
			return nil, 0, errors.New("unsupported search")
		}
		if id != "0" && !strings.HasPrefix(id, "view:") {
			return nil, 0, errors.New("unsupported search container")
		}
		if strings.HasPrefix(id, "view:") {
			views, e := i.host.Experience.Views(r.Context(), viewer)
			if e != nil {
				return nil, 0, e
			}
			found := false
			for _, view := range views {
				if "view:"+view.ID == id {
					query.Types = []frontend.MediaType{view.Type}
					found = true
					break
				}
			}
			if !found {
				return nil, 0, errors.New("unknown view")
			}
		}
		if strings.HasPrefix(id, "view:") {
			if text == "" {
				page, err = i.host.Experience.LibraryItems(r.Context(), viewer, device, strings.TrimPrefix(id, "view:"), query)
			} else {
				page, err = i.searchView(r, viewer, device, strings.TrimPrefix(id, "view:"), text, query)
			}
		} else {
			var result frontend.SearchPage
			result, err = i.host.Experience.Search(r.Context(), viewer, device, text, query)
			page = result.Page
		}
	} else if strings.HasPrefix(id, "view:") {
		page, err = i.host.Experience.LibraryItems(r.Context(), viewer, device, strings.TrimPrefix(id, "view:"), query)
	} else {
		key, e := objectKey(id)
		if e != nil {
			return nil, 0, e
		}
		page, err = i.host.Experience.Children(r.Context(), viewer, device, key, query)
	}
	if err != nil {
		return nil, 0, err
	}
	out := make([]didlObject, 0, len(page.Items))
	for _, title := range page.Items {
		out = append(out, objectFor(title, id))
	}
	return out, page.Total, nil
}

// searchView keeps a scoped search inside the selected library. The host
// returns only titles visible to this viewer; count matches before paging.
func (i *instance) searchView(r *http.Request, viewer *frontend.Viewer, device frontend.Device, view, text string, requested frontend.ItemQuery) (frontend.Page, error) {
	result := frontend.Page{}
	needle := strings.ToLower(text)
	for offset := 0; offset < 100000; {
		query := requested
		query.Offset, query.Limit = offset, 500
		page, err := i.host.Experience.LibraryItems(r.Context(), viewer, device, view, query)
		if err != nil {
			return frontend.Page{}, err
		}
		for _, title := range page.Items {
			if !strings.Contains(strings.ToLower(title.Title), needle) {
				continue
			}
			if result.Total >= requested.Offset && len(result.Items) < requested.Limit {
				result.Items = append(result.Items, title)
			}
			result.Total++
		}
		offset += len(page.Items)
		if offset >= page.Total {
			return result, nil
		}
		if len(page.Items) == 0 {
			return frontend.Page{}, errors.New("incomplete library listing")
		}
	}
	return frontend.Page{}, errors.New("library exceeds search bound")
}

func objectKey(value string) (frontend.RatingKey, error) {
	raw, ok := strings.CutPrefix(value, "item:")
	if !ok {
		return 0, errors.New("invalid ID")
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != raw {
		return 0, errors.New("invalid ID")
	}
	return frontend.RatingKey(n), nil
}
func searchText(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "*" {
		return "", true
	}
	prefix := `dc:title contains "`
	if !strings.HasPrefix(strings.ToLower(value), prefix) || !strings.HasSuffix(value, `"`) {
		return "", false
	}
	text := value[len(prefix) : len(value)-1]
	if len(text) > 256 || strings.ContainsAny(text, "\r\n\x00\"") {
		return "", false
	}
	return text, true
}
