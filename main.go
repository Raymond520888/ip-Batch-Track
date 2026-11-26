resetProgress := func(total int) {
		progMu.Lock()
		doneCount = 0
		totalCount = total
		progMu.Unlock()
		fyne.Do(func() {
			progressBar.SetValue(0)
			progressText.SetText(fmt.Sprintf("0/%d（0.0%%）", total))
		})
	}

	stepProgress := func() {
		progMu.Lock()
		doneCount++
		cur := doneCount
		tot := totalCount
		progMu.Unlock()

		var ratio float64
		if tot > 0 {
			ratio = float64(cur) / float64(tot)
			if ratio > 1 {
				ratio = 1
			}
		}
		pct := ratio * 100

		fyne.Do(func() {
			progressBar.SetValue(ratio)
			progressText.SetText(fmt.Sprintf("%d/%d（%.1f%%）", cur, tot, pct))
		})
	}