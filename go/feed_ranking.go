package isureal

import "sort"

// Keep original positions so equal scores retain the SQL/stable-sort order.
func topRankedPosts(posts []PostWithAuthor, scores map[string]int64, input getFeedRequest) []PostWithAuthor {
	if len(posts) <= 100 {
		sort.SliceStable(posts, func(i, j int) bool { return scores[posts[i].ID] > scores[posts[j].ID] })
		return posts
	}
	better := func(i, j int) bool {
		a, b := scores[posts[i].ID], scores[posts[j].ID]
		return a > b || a == b && i < j
	}
	selected := make([]int, 0, min(51, len(posts)))
	for i := range posts {
		if input.BeforeScore != nil && !rankedBefore(scores[posts[i].ID], posts[i].Post, *input.BeforeScore, *input.BeforePostedAt, input.BeforeID) {
			continue
		}
		if len(selected) < 51 {
			selected = append(selected, i)
			for child := len(selected) - 1; child > 0; {
				parent := (child - 1) / 2
				if !better(selected[parent], selected[child]) {
					break
				}
				selected[parent], selected[child] = selected[child], selected[parent]
				child = parent
			}
		} else if better(i, selected[0]) {
			selected[0] = i
			for parent := 0; ; {
				child := parent*2 + 1
				if child >= len(selected) {
					break
				}
				if child+1 < len(selected) && better(selected[child], selected[child+1]) {
					child++
				}
				if !better(selected[parent], selected[child]) {
					break
				}
				selected[parent], selected[child] = selected[child], selected[parent]
				parent = child
			}
		}
	}
	sort.Slice(selected, func(i, j int) bool { return better(selected[i], selected[j]) })
	result := make([]PostWithAuthor, len(selected))
	for i, index := range selected {
		result[i] = posts[index]
	}
	return result
}
