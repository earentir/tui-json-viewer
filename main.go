package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

var (
	// Regular expressions to match JSON elements
	keyRegex         = regexp.MustCompile(`"([^"]+)":\s*`)
	stringRegex      = regexp.MustCompile(`:\s*"([^"]*?)"`)
	arrayStringRegex = regexp.MustCompile(`\[\s*"([^"]*?)"\s*(,\s*"([^"]*?)"\s*)*]`)
	numberRegex      = regexp.MustCompile(`:\s*([-+]?\d*\.?\d+([eE][-+]?\d+)?)\s*[,}]`)
	arrayNumberRegex = regexp.MustCompile(`\[\s*([-+]?\d*\.?\d+([eE][-+]?\d+)?)(?:\s*,\s*[-+]?\d*\.?\d+([eE][-+]?\d+)?)*\s*\]`)
	booleanRegex     = regexp.MustCompile(`\b(true|false)\b`)
	nullRegex        = regexp.MustCompile(`:\s*(null)`)

	infoLogger  *log.Logger
	errorLogger *log.Logger
)

type searchOptions struct {
	caseSensitive bool
	useRegex      bool
}

type appState struct {
	app               *tview.Application
	fileList          *tview.List
	fileContent       *tview.TextView
	secondFileContent *tview.TextView
	debugView         *tview.TextView
	footer            *tview.TextView
	mainFlex          *tview.Flex
	rootLayout        *tview.Flex

	actionFuncs        []func()
	isFileListFocused  bool
	scrollOffset       int
	activeFileIndex    int
	layoutHorizontal   bool
	secondFileVisible  bool
	searchString       string
	searchResults      []struct{ line, pos int }
	currentSearchIndex int
	searchMode         bool
}

func main() {
	initLoggers()

	state := initializeApp()
	setupLayout(state)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	reloadJSONFiles(ctx, state.fileList, state.fileContent, state.debugView, ".", &state.actionFuncs, &state.activeFileIndex)

	state.setupKeyBindings()

	if err := state.app.SetRoot(state.rootLayout, true).Run(); err != nil {
		errorLogger.Printf("Application error: %v", err)
		panic(err)
	}
}

func containsAnyPosition(start, end int, positions []int) bool {
	for _, pos := range positions {
		if pos >= start && pos < end {
			return true
		}
	}
	return false
}

func colorizeJSON(input string) string {
	// Define color codes for JSON components
	keyColor := "[blue]"           // Color for all keys
	stringColor := "[lightgreen]"  // Color for string values
	arrayStringColor := "[green]"  // Color for string values in arrays
	numberColor := "[yellow]"      // Color for numeric values
	arrayNumberColor := "[yellow]" // Color for numeric values in arrays
	booleanColor := "[lightblue]"  // Color for boolean values (true/false)
	nullColor := "[red]"           // Color for null values
	resetColor := "[-]"            // Reset color to default

	// Apply color to keys
	input = keyRegex.ReplaceAllString(input, keyColor+"\"$1\""+resetColor+": ")

	// Apply color to string values
	input = stringRegex.ReplaceAllStringFunc(input, func(s string) string {
		// Match the colon and spaces before the string value
		colonIndex := s[:2]
		// Return colored string
		return colonIndex + stringColor + s[2:len(s)-1] + resetColor + s[len(s)-1:]
	})

	// Apply color to string values in arrays
	input = arrayStringRegex.ReplaceAllStringFunc(input, func(s string) string {
		// Match individual strings within the array and color them
		return regexp.MustCompile(`"(.*?)"`).ReplaceAllString(s, arrayStringColor+"\"$1\""+resetColor)
	})

	// Apply color to numeric values
	input = numberRegex.ReplaceAllStringFunc(input, func(s string) string {
		return s[:2] + numberColor + s[2:] + resetColor
	})

	// Apply color to numeric values in arrays
	input = arrayNumberRegex.ReplaceAllStringFunc(input, func(s string) string {
		// Apply color to each number within the matched array
		return regexp.MustCompile(`(\d+)`).ReplaceAllStringFunc(s, func(num string) string {
			return arrayNumberColor + num + resetColor
		})
	})

	// Apply color to boolean values
	input = booleanRegex.ReplaceAllStringFunc(input, func(s string) string {
		return booleanColor + s + resetColor
	})

	// Apply color to null values
	input = nullRegex.ReplaceAllStringFunc(input, func(s string) string {
		return s[:2] + nullColor + s[2:] + resetColor
	})

	return input
}

func executeCriticalOperation(debugView *tview.TextView, operation func() error) {
	defer recoverFromPanic(debugView)

	if err := operation(); err != nil {
		errorLogger.Printf("Error in critical operation: %v", err)
		debugView.SetText("[red]Error in critical operation. Check error log for details.[-]")
	}
}

func handleScroll(event *tcell.EventKey, firstContent, secondContent *tview.TextView, scrollOffset *int) {
	if secondContent == nil {
		return // No need to sync if there's no second content view
	}

	switch event.Key() {
	case tcell.KeyUp:
		*scrollOffset--
		if *scrollOffset < 0 {
			*scrollOffset = 0
		}
	case tcell.KeyDown:
		*scrollOffset++
	case tcell.KeyPgUp:
		*scrollOffset -= 10
		if *scrollOffset < 0 {
			*scrollOffset = 0
		}
	case tcell.KeyPgDn:
		*scrollOffset += 10
	}

	// Apply the scroll offset to both content views
	firstContent.ScrollTo(*scrollOffset, 0)
	secondContent.ScrollTo(*scrollOffset, 0)
}

func highlightSearchResult(fileContent *tview.TextView, searchPositions []int, searchString string) {
	if len(searchPositions) == 0 {
		return
	}

	content := fileContent.GetText(true)
	lines := strings.Split(content, "\n")

	var highlightedContent strings.Builder
	currentPosition := 0

	for i, line := range lines {
		lineStart := currentPosition
		lineEnd := lineStart + len(line)

		if containsAnyPosition(lineStart, lineEnd, searchPositions) {
			highlightedContent.WriteString("[::b]" + line + "[::-]") // Use bold text as highlight
		} else {
			highlightedContent.WriteString(line)
		}

		if i < len(lines)-1 {
			highlightedContent.WriteString("\n")
		}

		currentPosition = lineEnd + 1 // +1 for the newline character
	}

	fileContent.SetText(highlightedContent.String())
}

func initializeApp() *appState {
	state := &appState{
		app:               tview.NewApplication(),
		isFileListFocused: true,
		layoutHorizontal:  true,
	}

	state.fileList = tview.NewList().ShowSecondaryText(false)
	state.fileList.SetBorder(true).SetBorderColor(tcell.ColorGreen).SetTitle("Files")

	state.fileContent = tview.NewTextView().SetDynamicColors(true).SetWrap(true).SetScrollable(true)
	state.fileContent.SetBorder(true).SetBorderColor(tcell.ColorGray).SetTitle("Content")

	state.debugView = tview.NewTextView().SetDynamicColors(true).SetWrap(true)
	state.debugView.SetText("Press F1, ?, or h for help. Press q to quit.")

	state.footer = tview.NewTextView().SetText("F1/?/h - Help, qQ - Quit, / - Search")
	state.footer.SetDynamicColors(true).SetTextAlign(tview.AlignCenter)

	return state
}

func initLoggers() {
	infoFile, err := os.OpenFile("info.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		log.Fatal("Failed to open info log file:", err)
	}

	errorFile, err := os.OpenFile("error.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		log.Fatal("Failed to open error log file:", err)
	}

	infoLogger = log.New(infoFile, "INFO: ", log.Ldate|log.Ltime|log.Lshortfile)
	errorLogger = log.New(errorFile, "ERROR: ", log.Ldate|log.Ltime|log.Lshortfile)
}

func loadFileContentAsync(app *tview.Application, file string, content *tview.TextView, debugView *tview.TextView, wg *sync.WaitGroup) {
	defer wg.Done()

	fileContent, err := readFileContent(file)
	if err != nil {
		app.QueueUpdateDraw(func() {
			errorLogger.Printf("Failed to read file %s: %v", file, err)
			debugView.SetText("[red]Failed to read file. Check error log for details.[-]")
		})
		return
	}

	var formattedContent map[string]interface{}
	if err := json.Unmarshal([]byte(fileContent), &formattedContent); err != nil {
		app.QueueUpdateDraw(func() {
			errorLogger.Printf("Invalid JSON in file %s: %v", file, err)
			debugView.SetText("[red]Invalid JSON. Check error log for details.[-]")
		})
		return
	}

	prettyContent, _ := json.MarshalIndent(formattedContent, "", "  ")
	coloredContent := colorizeJSON(string(prettyContent))

	app.QueueUpdateDraw(func() {
		content.SetText(coloredContent).SetTitle(filepath.Base(file))
	})
}

func loadJSONFilesWithContext(ctx context.Context, dir string) ([]string, error) {
	var files []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			if err != nil {
				return fmt.Errorf("error accessing path %s: %w", path, err)
			}
			if !info.IsDir() && filepath.Ext(info.Name()) == ".json" {
				files = append(files, path)
			}
			return nil
		}
	})
	if err != nil {
		return nil, fmt.Errorf("error walking directory %s: %w", dir, err)
	}
	return files, nil
}

func performSearch(content, searchString string, options searchOptions) []int {
	var positions []int

	if options.useRegex {
		var re *regexp.Regexp
		var err error
		if options.caseSensitive {
			re, err = regexp.Compile(searchString)
		} else {
			re, err = regexp.Compile("(?i)" + searchString)
		}
		if err != nil {
			errorLogger.Printf("Invalid regex pattern: %v", err)
			return positions
		}

		matches := re.FindAllStringIndex(content, -1)
		for _, match := range matches {
			positions = append(positions, match[0])
		}
	} else {
		if !options.caseSensitive {
			content = strings.ToLower(content)
			searchString = strings.ToLower(searchString)
		}

		pos := 0
		for {
			index := strings.Index(content[pos:], searchString)
			if index == -1 {
				break
			}
			positions = append(positions, pos+index)
			pos += index + len(searchString)
		}
	}

	return positions
}

func recoverFromPanic(debugView *tview.TextView) {
	if r := recover(); r != nil {
		errorMsg := fmt.Sprintf("Recovered from panic: %v", r)
		if errorLogger != nil {
			errorLogger.Println(errorMsg)
		} else {
			log.Println(errorMsg) // Fallback to standard logger if errorLogger is not initialized
		}
		if debugView != nil {
			debugView.SetText("[red]" + errorMsg + "[-]")
		}
	}
}

func readFileContent(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	var content strings.Builder
	reader := bufio.NewReader(file)
	buffer := make([]byte, 1024)

	for {
		n, err := reader.Read(buffer)
		if err != nil && err != io.EOF {
			return "", fmt.Errorf("error reading file: %w", err)
		}
		if n == 0 {
			break
		}
		content.Write(buffer[:n])
	}

	return content.String(), nil
}

// reloadJSONFiles loads the list of JSON files in the specified directory and updates the UI.
func reloadJSONFiles(ctx context.Context, fileList *tview.List, fileContent, debugView *tview.TextView, dir string, actionFuncs *[]func(), activeFileIndex *int) {
	fileList.Clear()
	*actionFuncs = nil // Reset action functions slice

	jsonFiles, err := loadJSONFilesWithContext(ctx, dir)
	if err != nil {
		errorLogger.Printf("Failed to load JSON files: %v", err)
		debugView.SetText("[red]Failed to load JSON files. Check error log for details.[-]")
		return
	}

	// Populate the list with JSON files
	for i, file := range jsonFiles {
		file := file // capture range variable
		fileIndex := i
		action := func() {
			content, err := readFileContent(file)
			if err != nil {
				errorLogger.Printf("Failed to read file %s: %v", file, err)
				debugView.SetText("[red]Failed to read file. Check error log for details.[-]")
				return
			}

			var formattedContent map[string]interface{}
			if err := json.Unmarshal([]byte(content), &formattedContent); err != nil {
				errorLogger.Printf("Invalid JSON in file %s: %v", file, err)
				debugView.SetText("[red]Invalid JSON. Check error log for details.[-]")
				return
			}

			prettyContent, _ := json.MarshalIndent(formattedContent, "", "  ")
			coloredContent := colorizeJSON(string(prettyContent))

			fileContent.SetText(coloredContent).SetTitle(filepath.Base(file))

			*activeFileIndex = fileIndex
			updateActiveFileHighlight(fileList, *activeFileIndex)
		}
		fileList.AddItem(file, "", 0, action)
		*actionFuncs = append(*actionFuncs, action)
	}

	infoLogger.Println("JSON files loaded successfully")
	debugView.SetText("Select a file to view its content.")
}

func setupLayout(state *appState) {
	state.mainFlex = tview.NewFlex().
		AddItem(state.fileList, 0, 1, true).
		AddItem(state.fileContent, 0, 2, false)

	state.rootLayout = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(state.mainFlex, 0, 1, true).
		AddItem(state.debugView, 1, 1, false).
		AddItem(state.footer, 1, 1, false)
}

func toggleCompareView(app *tview.Application, firstContent *tview.TextView, secondContent **tview.TextView, secondVisible *bool, mainFlex *tview.Flex, fileList *tview.List, actionFuncs []func(), debugView *tview.TextView) {
	// Get the index of the selected file
	selectedFileIndex := fileList.GetCurrentItem()

	// Check if the second panel is currently visible
	if *secondVisible {
		// Hide the second panel
		mainFlex.RemoveItem(*secondContent)
		*secondVisible = false
	} else {
		// Show the second panel
		if *secondContent == nil {
			// Creating second content view
			*secondContent = tview.NewTextView().SetDynamicColors(true).SetWrap(true).SetScrollable(true)
			(*secondContent).SetBorder(true).SetBorderColor(tcell.ColorGray)
		}

		// Load the content of the selected file into the second panel without affecting the main content pane
		mainText, _ := fileList.GetItemText(selectedFileIndex) // Get the main text of the selected item

		content, err := readFileContent(filepath.Join(".", mainText))
		if err != nil {
			errorLogger.Printf("Failed to read file %s: %v", mainText, err)
			debugView.SetText("[red]Failed to read file. Check error log for details.[-]")
			return
		}

		var formattedContent map[string]interface{}
		if err := json.Unmarshal([]byte(content), &formattedContent); err != nil {
			errorLogger.Printf("Invalid JSON in file %s: %v", mainText, err)
			debugView.SetText("[red]Invalid JSON. Check error log for details.[-]")
			return
		}

		prettyContent, _ := json.MarshalIndent(formattedContent, "", "  ")
		coloredContent := colorizeJSON(string(prettyContent)) // Apply color to both keys and values

		(*secondContent).SetText(coloredContent)

		// Update the title to the filename
		(*secondContent).SetTitle(mainText)

		// Adding second panel to layout
		mainFlex.AddItem(*secondContent, 0, 2, false)
		*secondVisible = true
	}
}

// updateActiveFileHighlight updates the color of the active file in the file list
func updateActiveFileHighlight(fileList *tview.List, activeFileIndex int) {
	// Regular expression to match color codes in square brackets
	re := regexp.MustCompile(`\[[^\]]*\]`)

	for i := 0; i < fileList.GetItemCount(); i++ {
		mainText, _ := fileList.GetItemText(i)
		// Replace color codes with an empty string to remove them
		plainString := re.ReplaceAllString(mainText, "")
		fileList.SetItemText(i, plainString, "") // Highlight active file in light green
	}

	for i := 0; i < fileList.GetItemCount(); i++ {
		mainText, _ := fileList.GetItemText(i)
		if i == activeFileIndex {
			fileList.SetItemText(i, "[lightgreen]"+mainText+"[-]", "") // Highlight active file in light green
		}
	}
}

// updatePaneFocus updates the border style of the focused pane
func updatePaneFocus(fileList *tview.List, fileContent *tview.TextView, isFileListFocused bool) {
	if isFileListFocused {
		fileList.SetBorder(true).SetBorderColor(tcell.ColorGreen).SetTitle("Files")
		fileContent.SetBorder(true).SetBorderColor(tcell.ColorGray)
	} else {
		fileList.SetBorder(true).SetBorderColor(tcell.ColorGray).SetTitle("Files")
		fileContent.SetBorder(true).SetBorderColor(tcell.ColorGreen)
	}
}

func (state *appState) cancelSearch() {
	state.searchMode = false
	state.searchString = ""
	state.searchResults = nil
	state.currentSearchIndex = 0
	state.fileContent.Highlight("")
	state.debugView.SetText("")
	state.app.SetFocus(state.fileContent)
}

func (state *appState) findNextResult() {
	if len(state.searchResults) == 0 {
		return
	}
	state.currentSearchIndex = (state.currentSearchIndex + 1) % len(state.searchResults)
	state.highlightCurrentResult()
	state.debugView.SetText(fmt.Sprintf("Result %d of %d", state.currentSearchIndex+1, len(state.searchResults)))
}

func (state *appState) findPreviousResult() {
	if len(state.searchResults) == 0 {
		return
	}
	state.currentSearchIndex = (state.currentSearchIndex - 1 + len(state.searchResults)) % len(state.searchResults)
	state.highlightCurrentResult()
	state.debugView.SetText(fmt.Sprintf("Result %d of %d", state.currentSearchIndex+1, len(state.searchResults)))
}

func (state *appState) highlightCurrentResult() {
	if len(state.searchResults) == 0 {
		return
	}
	result := state.searchResults[state.currentSearchIndex]
	state.fileContent.Highlight(strconv.Itoa(result.line))
	state.fileContent.ScrollTo(result.pos, result.line)
}

func (state *appState) performSearch() {
	if state.searchString == "" {
		return
	}
	content := state.fileContent.GetText(true)
	lines := strings.Split(content, "\n")
	state.searchResults = nil
	for i, line := range lines {
		index := strings.Index(line, state.searchString)
		for index != -1 {
			state.searchResults = append(state.searchResults, struct{ line, pos int }{i, index})
			index = strings.Index(line[index+len(state.searchString):], state.searchString)
			if index != -1 {
				index += len(state.searchString)
			}
		}
	}
	state.currentSearchIndex = 0
	if len(state.searchResults) > 0 {
		state.highlightCurrentResult()
		state.debugView.SetText(fmt.Sprintf("Found %d occurrences. Result 1 of %d. Press 'n' for next, 'N' for previous.", len(state.searchResults), len(state.searchResults)))
	} else {
		state.debugView.SetText("[red]No results found for: " + state.searchString + "[-]")
	}
}

func (state *appState) setupKeyBindings() {
	state.app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if state.searchMode {
			switch event.Key() {
			case tcell.KeyEsc:
				state.cancelSearch()
			case tcell.KeyEnter:
				state.searchMode = false
				state.performSearch()
				state.app.SetFocus(state.fileContent)
			case tcell.KeyRune:
				state.searchString += string(event.Rune())
				state.debugView.SetText("Search: " + state.searchString)
			}
			return nil
		}

		switch event.Key() {
		case tcell.KeyRight, tcell.KeyLeft:
			state.isFileListFocused = !state.isFileListFocused
			updatePaneFocus(state.fileList, state.fileContent, state.isFileListFocused)
			if state.isFileListFocused {
				state.app.SetFocus(state.fileList)
				state.fileList.SetCurrentItem(state.activeFileIndex)
			} else {
				state.app.SetFocus(state.fileContent)
			}
		case tcell.KeyUp, tcell.KeyDown, tcell.KeyPgUp, tcell.KeyPgDn:
			if !state.isFileListFocused {
				handleScroll(event, state.fileContent, state.secondFileContent, &state.scrollOffset)
			}
		case tcell.KeyTab:
			state.isFileListFocused = !state.isFileListFocused
			updatePaneFocus(state.fileList, state.fileContent, state.isFileListFocused)
			if state.isFileListFocused {
				state.app.SetFocus(state.fileList)
				state.fileList.SetCurrentItem(state.activeFileIndex)
			} else {
				state.app.SetFocus(state.fileContent)
			}
		case tcell.KeyEnter:
			if state.isFileListFocused {
				state.activeFileIndex = state.fileList.GetCurrentItem()
				state.actionFuncs[state.activeFileIndex]()
				updateActiveFileHighlight(state.fileList, state.activeFileIndex)
				state.isFileListFocused = false
				updatePaneFocus(state.fileList, state.fileContent, state.isFileListFocused)
				state.app.SetFocus(state.fileContent)
			}
		case tcell.KeyRune:
			switch event.Rune() {
			case 'q', 'Q':
				state.app.Stop()
			case 'r', 'R':
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				reloadJSONFiles(ctx, state.fileList, state.fileContent, state.debugView, ".", &state.actionFuncs, &state.activeFileIndex)
			case 'c', 'C':
				toggleCompareView(state.app, state.fileContent, &state.secondFileContent, &state.secondFileVisible, state.mainFlex, state.fileList, state.actionFuncs, state.debugView)
			case 'o', 'O':
				state.toggleLayout()
			case 'f', 'F', '?', 'h', 'H':
				state.showHelpModal()
			case '/':
				state.startSearch()
			case 'n':
				state.findNextResult()
			case 'N':
				state.findPreviousResult()
			}
		}
		return event
	})
}

func (state *appState) showHelpModal() {
	helpText := `Shortcuts:
- F1/?/h: Show this help
- q/Q: Quit
- Arrows: Navigate between files and content
- Enter: Open selected file
- r/R: Reload files
- c/C: Compare files
- o/O: Toggle layout
- Tab: Switch focus
- /: Search
- n: Next search result
- N: Previous search result
- Esc: Cancel search`

	modal := tview.NewModal().
		SetText(helpText).
		AddButtons([]string{"Close"}).
		SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			state.app.SetRoot(state.rootLayout, true).SetFocus(state.fileList)
		})
	state.app.SetRoot(modal, true).SetFocus(modal)
}

func (state *appState) startSearch() {
	state.searchMode = true
	state.searchString = ""
	state.debugView.SetText("Search: ")
	state.app.SetFocus(state.debugView)
}

func (state *appState) toggleLayout() {
	if state.secondFileVisible {
		if state.layoutHorizontal {
			state.mainFlex.Clear()
			state.mainFlex.SetDirection(tview.FlexRow).
				AddItem(state.fileList, 0, 1, true).
				AddItem(state.fileContent, 0, 1, false).
				AddItem(state.secondFileContent, 0, 1, false)
			state.layoutHorizontal = false
		} else {
			state.mainFlex.Clear()
			state.mainFlex.SetDirection(tview.FlexColumn).
				AddItem(state.fileList, 0, 1, true).
				AddItem(state.fileContent, 0, 2, false).
				AddItem(state.secondFileContent, 0, 2, false)
			state.layoutHorizontal = true
		}
		state.app.ForceDraw()
	}
}
