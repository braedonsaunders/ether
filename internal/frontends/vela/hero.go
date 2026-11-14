package vela

// heroesFromRows uses only the personalized Experience's explicit featured
// presentation. A ready arbitrary shelf is not a featured source. Entries have
// already passed viewer policy and retain their server-issued item receipts.
func heroesFromRows(rows []object) ([]object, []object) {
	heroes := []object{}
	chosen := map[string]bool{}
	for _, row := range rows {
		if row["style"] != "hero" {
			continue
		}
		entries, _ := row["items"].([]object)
		for _, entry := range entries {
			if len(heroes) >= 5 {
				break
			}
			receipt, _ := entry["receiptToken"].(string)
			title, ok := entry["title"].(object)
			if entry["kind"] != "title" || receipt == "" || !ok {
				continue
			}
			id, _ := title["id"].(string)
			if id == "" || chosen[id] {
				continue
			}
			// Extras discovery/resolution is not a cached playback capability. This
			// source supplies no cached trailer grant, so the optional trailer stays
			// absent rather than triggering background or request-path resolution.
			hero := object{"title": title, "receiptToken": receipt}
			if enabled, _ := row["previewOnFocus"].(bool); enabled {
				hero["previewOnFocus"] = true
			}
			heroes = append(heroes, hero)
			chosen[id] = true
		}
	}
	if len(heroes) == 0 {
		return nil, rows
	}
	shelves := make([]object, 0, len(rows))
	for _, row := range rows {
		entries, _ := row["items"].([]object)
		kept := make([]object, 0, len(entries))
		for _, entry := range entries {
			if entry["kind"] == "title" {
				if title, ok := entry["title"].(object); ok {
					if id, _ := title["id"].(string); chosen[id] {
						continue
					}
				}
			}
			kept = append(kept, entry)
		}
		// Keep the original issued cursor: its offset includes entries now shown
		// in heroes. Recomputing it from the shorter shelf would repeat cards.
		if len(kept) == 0 && row["continuation"] == "exhausted" {
			continue
		}
		shelf := object{}
		for key, value := range row {
			shelf[key] = value
		}
		shelf["items"] = kept
		shelves = append(shelves, shelf)
	}
	return heroes, shelves
}
