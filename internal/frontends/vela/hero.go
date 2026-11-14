package vela

// heroFromRows reuses an authorized personalized entry and its issued receipt.
// It does not resolve artwork, invent attribution, or start trailer work.
func heroFromRows(rows []object) object {
	for _, heroRowsOnly := range []bool{true, false} {
		for _, row := range rows {
			if heroRowsOnly && row["style"] != "hero" {
				continue
			}
			entries, _ := row["items"].([]object)
			for _, entry := range entries {
				receipt, _ := entry["receiptToken"].(string)
				if entry["kind"] != "title" || receipt == "" {
					continue
				}
				title, ok := entry["title"].(object)
				if !ok {
					continue
				}
				artwork, _ := title["artwork"].([]object)
				for _, art := range artwork {
					sizes, _ := art["sizes"].([]object)
					for _, size := range sizes {
						if url, ok := size["url"].(string); ok && url != "" {
							return object{"title": title, "receiptToken": receipt}
						}
					}
				}
			}
		}
	}
	return nil
}
