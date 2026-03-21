package main

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Note struct {
	Title   string
	Content string
	Section string
	Path    string
}

type PageData struct {
	Title    string
	Sections map[string][]Note
	Query    string
	Flash    string
}

type SectionEditData struct {
	Title       string
	Section     string
	Notes       []Note
	AllSections map[string][]Note
	Flash       string
}

var (
	notesCache map[string][]Note
	cacheMutex sync.RWMutex
)

// loggingResponseWriter перехватывает статус ответа для логирования
type loggingResponseWriter struct {
	http.ResponseWriter
	status int
}

func (lrw *loggingResponseWriter) WriteHeader(code int) {
	lrw.status = code
	lrw.ResponseWriter.WriteHeader(code)
}

// loggingMiddleware логирует каждый HTTP-запрос
func loggingMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lw := &loggingResponseWriter{ResponseWriter: w, status: http.StatusOK}
		next(lw, r)
		duration := time.Since(start)
		log.Printf("[%s] %s %s - %d (%v)", r.Method, r.URL.Path, r.RemoteAddr, lw.status, duration)
	}
}

func main() {
	os.MkdirAll("notes", 0755)
	os.MkdirAll("templates", 0755)
	os.MkdirAll("static", 0755)

	loadNotesCached()

	http.HandleFunc("/", loggingMiddleware(indexHandler))
	http.HandleFunc("/view/", loggingMiddleware(viewHandler))
	http.HandleFunc("/search", loggingMiddleware(searchHandler))
	http.HandleFunc("/create", loggingMiddleware(createHandler))
	http.HandleFunc("/save", loggingMiddleware(saveHandler))
	http.HandleFunc("/edit/", loggingMiddleware(editHandler))
	http.HandleFunc("/update", loggingMiddleware(updateHandler))
	http.HandleFunc("/delete/", loggingMiddleware(deleteHandler))
	http.HandleFunc("/delete-section/", loggingMiddleware(deleteSectionHandler))
	http.HandleFunc("/edit-section/", loggingMiddleware(editSectionHandler))
	http.HandleFunc("/rename-section", loggingMiddleware(renameSectionHandler))
	http.HandleFunc("/move-note", loggingMiddleware(moveNoteHandler))

	http.HandleFunc("/import-export", loggingMiddleware(importExportHandler))
	http.HandleFunc("/api/categories", loggingMiddleware(categoriesHandler))
	http.HandleFunc("/export/txt", loggingMiddleware(exportTxtHandler))
	http.HandleFunc("/export/doc", loggingMiddleware(exportDocHandler))
	http.HandleFunc("/import", loggingMiddleware(importHandler))

	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/static/", http.StripPrefix("/static/", fs))

	log.Println("Сервер запущен на http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func loadNotes() map[string][]Note {
	notes := make(map[string][]Note)
	err := filepath.Walk("notes", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == "notes" || info.IsDir() || filepath.Ext(path) != ".txt" {
			return nil
		}
		relPath, err := filepath.Rel("notes", path)
		if err != nil {
			return err
		}
		section := filepath.Dir(relPath)
		if section == "." {
			section = "Общие"
		}
		content, err := os.ReadFile(path)
		if err != nil {
			log.Printf("Ошибка чтения файла %s: %v", path, err)
			return nil
		}
		title := strings.TrimSuffix(filepath.Base(path), ".txt")
		note := Note{
			Title:   title,
			Content: string(content),
			Section: section,
			Path:    strings.TrimSuffix(relPath, ".txt"),
		}
		notes[section] = append(notes[section], note)
		return nil
	})
	if err != nil {
		log.Printf("Ошибка загрузки заметок: %v", err)
	}
	return notes
}

func loadNotesCached() map[string][]Note {
	cacheMutex.RLock()
	if notesCache != nil {
		defer cacheMutex.RUnlock()
		return notesCache
	}
	cacheMutex.RUnlock()
	cacheMutex.Lock()
	defer cacheMutex.Unlock()
	if notesCache == nil {
		notesCache = loadNotes()
	}
	return notesCache
}

func updateNotesCache() {
	cacheMutex.Lock()
	defer cacheMutex.Unlock()
	notesCache = loadNotes()
}

func setFlash(w http.ResponseWriter, message string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "flash",
		Value:    url.QueryEscape(message),
		Path:     "/",
		MaxAge:   1,
		HttpOnly: true,
	})
}

func getFlash(w http.ResponseWriter, r *http.Request) string {
	cookie, err := r.Cookie("flash")
	if err != nil {
		return ""
	}
	message, _ := url.QueryUnescape(cookie.Value)
	http.SetCookie(w, &http.Cookie{
		Name:     "flash",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
	})
	return message
}

func isValidFilename(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	forbiddenChars := `<>:"/\|?*`
	return !strings.ContainsAny(name, forbiddenChars)
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	notes := loadNotesCached()
	data := PageData{
		Title:    "NoteZone",
		Sections: notes,
		Flash:    getFlash(w, r),
	}
	tmpl := template.Must(template.ParseFiles("templates/index.html"))
	err := tmpl.Execute(w, data)
	if err != nil {
		log.Printf("Ошибка рендеринга шаблона index.html: %v", err)
		http.Error(w, "Внутренняя ошибка сервера", http.StatusInternalServerError)
	}
}

func viewHandler(w http.ResponseWriter, r *http.Request) {
	notePath := strings.TrimPrefix(r.URL.Path, "/view/")
	if notePath == "" {
		http.NotFound(w, r)
		return
	}
	fullPath := filepath.Join("notes", notePath+".txt")
	if !strings.HasPrefix(filepath.Clean(fullPath), filepath.Clean("notes")+string(os.PathSeparator)) {
		http.Error(w, "Недопустимый путь", http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		http.NotFound(w, r)
		return
	}
	content, err := os.ReadFile(fullPath)
	if err != nil {
		log.Printf("Ошибка чтения файла %s: %v", fullPath, err)
		http.Error(w, "Ошибка чтения файла", http.StatusInternalServerError)
		return
	}
	section := filepath.Dir(notePath)
	if section == "." {
		section = "Общие"
	}
	title := strings.TrimSuffix(filepath.Base(notePath), ".txt")
	note := Note{
		Title:   title,
		Content: string(content),
		Section: section,
		Path:    notePath,
	}
	data := struct {
		Note
		Flash string
	}{
		Note:  note,
		Flash: getFlash(w, r),
	}
	tmpl := template.Must(template.ParseFiles("templates/view.html"))
	err = tmpl.Execute(w, data)
	if err != nil {
		log.Printf("Ошибка рендеринга шаблона view.html: %v", err)
		http.Error(w, "Внутренняя ошибка сервера", http.StatusInternalServerError)
	}
}

func deleteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		log.Printf("Неподдерживаемый метод %s для /delete/", r.Method)
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	notePath := strings.TrimPrefix(r.URL.Path, "/delete/")
	if notePath == "" {
		http.NotFound(w, r)
		return
	}
	fullPath := filepath.Join("notes", notePath+".txt")
	if !strings.HasPrefix(filepath.Clean(fullPath), filepath.Clean("notes")+string(os.PathSeparator)) {
		http.Error(w, "Недопустимый путь", http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		http.NotFound(w, r)
		return
	}
	log.Printf("Удаление заметки: %s", fullPath)
	err := os.Remove(fullPath)
	if err != nil {
		log.Printf("Ошибка удаления файла %s: %v", fullPath, err)
		http.Error(w, "Ошибка удаления файла", http.StatusInternalServerError)
		return
	}
	updateNotesCache()
	setFlash(w, "Заметка успешно удалена")
	log.Printf("Заметка успешно удалена: %s", fullPath)
	w.WriteHeader(http.StatusOK)
}

func createHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	notes := loadNotesCached()
	sections := make([]string, 0, len(notes))
	for section := range notes {
		sections = append(sections, section)
	}
	funcMap := template.FuncMap{
		"js": func(s interface{}) string {
			str := template.JSEscapeString(fmt.Sprintf("%v", s))
			return str
		},
	}
	data := map[string]interface{}{
		"Title":    "Создать новую заметку",
		"Sections": sections,
		"Flash":    getFlash(w, r),
	}
	tmpl := template.Must(template.New("create.html").Funcs(funcMap).ParseFiles("templates/create.html"))
	err := tmpl.Execute(w, data)
	if err != nil {
		log.Printf("Ошибка рендеринга шаблона create.html: %v", err)
		http.Error(w, "Внутренняя ошибка сервера", http.StatusInternalServerError)
	}
}

func saveHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	err := r.ParseForm()
	if err != nil {
		log.Printf("Ошибка парсинга формы при сохранении: %v", err)
		http.Error(w, "Ошибка обработки формы", http.StatusBadRequest)
		return
	}
	section := r.FormValue("section")
	newSection := r.FormValue("new_section")
	title := r.FormValue("title")
	content := r.FormValue("content")
	finalSection := section
	if section == "" && newSection != "" {
		finalSection = newSection
	}
	if finalSection == "" || title == "" || content == "" {
		http.Error(w, "Все поля обязательны для заполнения", http.StatusBadRequest)
		return
	}
	finalSection = strings.Trim(finalSection, "/\\")
	if finalSection == "." {
		finalSection = "Общие"
	}
	if !isValidFilename(title) || !isValidFilename(finalSection) {
		http.Error(w, "Недопустимые символы в названии", http.StatusBadRequest)
		return
	}
	sectionPath := filepath.Join("notes", finalSection)
	err = os.MkdirAll(sectionPath, 0755)
	if err != nil {
		log.Printf("Ошибка создания раздела %s: %v", sectionPath, err)
		http.Error(w, "Ошибка создания раздела: "+err.Error(), http.StatusInternalServerError)
		return
	}
	filePath := filepath.Join(sectionPath, title+".txt")
	if _, err := os.Stat(filePath); err == nil {
		http.Error(w, "Заметка с таким названием уже существует", http.StatusBadRequest)
		return
	}
	log.Printf("Создание заметки: раздел=%s, название=%s", finalSection, title)
	err = os.WriteFile(filePath, []byte(content), 0644)
	if err != nil {
		log.Printf("Ошибка сохранения файла %s: %v", filePath, err)
		http.Error(w, "Ошибка сохранения файла: "+err.Error(), http.StatusInternalServerError)
		return
	}
	updateNotesCache()
	setFlash(w, "Заметка успешно создана")
	log.Printf("Заметка успешно создана: %s", filePath)
	newPath := filepath.Join(finalSection, title)
	http.Redirect(w, r, "/view/"+newPath, http.StatusSeeOther)
}

func editHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	notePath := strings.TrimPrefix(r.URL.Path, "/edit/")
	if notePath == "" {
		http.NotFound(w, r)
		return
	}
	fullPath := filepath.Join("notes", notePath+".txt")
	if !strings.HasPrefix(filepath.Clean(fullPath), filepath.Clean("notes")+string(os.PathSeparator)) {
		http.Error(w, "Недопустимый путь", http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		http.NotFound(w, r)
		return
	}
	content, err := os.ReadFile(fullPath)
	if err != nil {
		log.Printf("Ошибка чтения файла %s: %v", fullPath, err)
		http.Error(w, "Ошибка чтения файла", http.StatusInternalServerError)
		return
	}
	section := filepath.Dir(notePath)
	if section == "." {
		section = "Общие"
	}
	title := strings.TrimSuffix(filepath.Base(notePath), ".txt")
	notes := loadNotesCached()
	sections := make([]string, 0, len(notes))
	for s := range notes {
		sections = append(sections, s)
	}
	funcMap := template.FuncMap{
		"contains": func(slice []string, item string) bool {
			for _, s := range slice {
				if s == item {
					return true
				}
			}
			return false
		},
		"js": func(s interface{}) string {
			str := template.JSEscapeString(fmt.Sprintf("%v", s))
			return str
		},
	}
	data := map[string]interface{}{
		"Title":     "Редактировать заметку",
		"Sections":  sections,
		"Section":   section,
		"NoteTitle": title,
		"Content":   string(content),
		"NotePath":  notePath,
		"Flash":     getFlash(w, r),
	}
	tmpl := template.Must(template.New("edit.html").Funcs(funcMap).ParseFiles("templates/edit.html"))
	err = tmpl.Execute(w, data)
	if err != nil {
		log.Printf("Ошибка рендеринга шаблона edit.html: %v", err)
		http.Error(w, "Внутренняя ошибка сервера", http.StatusInternalServerError)
	}
}

func updateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	err := r.ParseForm()
	if err != nil {
		log.Printf("Ошибка парсинга формы при обновлении: %v", err)
		http.Error(w, "Ошибка обработки формы", http.StatusBadRequest)
		return
	}
	oldPath := r.FormValue("old_path")
	section := r.FormValue("section")
	newSection := r.FormValue("new_section")
	title := r.FormValue("title")
	content := r.FormValue("content")
	finalSection := strings.TrimSpace(section)
	if finalSection == "" {
		finalSection = strings.TrimSpace(newSection)
	}
	title = strings.TrimSpace(title)
	content = strings.TrimSpace(content)
	finalSection = strings.TrimSpace(finalSection)
	if finalSection == "" {
		log.Printf("Ошибка валидации: finalSection пусто")
		http.Error(w, "Раздел не может быть пустым", http.StatusBadRequest)
		return
	}
	if title == "" {
		log.Printf("Ошибка валидации: title пусто")
		http.Error(w, "Название не может быть пустым", http.StatusBadRequest)
		return
	}
	if content == "" {
		log.Printf("Ошибка валидации: content пусто")
		http.Error(w, "Содержимое не может быть пустым", http.StatusBadRequest)
		return
	}
	finalSection = strings.Trim(finalSection, "/\\")
	if finalSection == "." {
		finalSection = "Общие"
	}
	if !isValidFilename(title) {
		log.Printf("Ошибка валидации: недопустимое название '%s'", title)
		http.Error(w, "Недопустимые символы в названии заметки", http.StatusBadRequest)
		return
	}
	if !isValidFilename(finalSection) {
		log.Printf("Ошибка валидации: недопустимое название раздела '%s'", finalSection)
		http.Error(w, "Недопустимые символы в названии раздела", http.StatusBadRequest)
		return
	}
	sectionPath := filepath.Join("notes", finalSection)
	err = os.MkdirAll(sectionPath, 0755)
	if err != nil {
		log.Printf("Ошибка создания папки %s: %v", sectionPath, err)
		http.Error(w, "Ошибка создания раздела", http.StatusInternalServerError)
		return
	}
	newFullPath := filepath.Join(sectionPath, title+".txt")
	oldFullPath := filepath.Join("notes", oldPath+".txt")
	if oldFullPath != newFullPath {
		if _, err := os.Stat(newFullPath); err == nil {
			log.Printf("Ошибка: файл уже существует %s", newFullPath)
			http.Error(w, "Заметка с таким названием уже существует", http.StatusBadRequest)
			return
		}
	}
	log.Printf("Обновление заметки: старая_путь=%s, новый_раздел=%s, новое_название=%s", oldPath, finalSection, title)
	if oldFullPath != newFullPath {
		err := os.Remove(oldFullPath)
		if err != nil && !os.IsNotExist(err) {
			log.Printf("Ошибка удаления старого файла %s: %v", oldFullPath, err)
			http.Error(w, "Ошибка при удалении старого файла", http.StatusInternalServerError)
			return
		}
	}
	err = os.WriteFile(newFullPath, []byte(content), 0644)
	if err != nil {
		log.Printf("Ошибка сохранения файла %s: %v", newFullPath, err)
		http.Error(w, "Ошибка сохранения файла", http.StatusInternalServerError)
		return
	}
	updateNotesCache()
	log.Printf("Заметка успешно обновлена: %s", newFullPath)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Заметка успешно обновлена"))
}

func searchHandler(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	log.Printf("Поиск по запросу: %s", query)
	notes := loadNotesCached()
	results := make(map[string][]Note)
	for section, sectionNotes := range notes {
		for _, note := range sectionNotes {
			if strings.Contains(strings.ToLower(note.Title+note.Content), strings.ToLower(query)) {
				results[section] = append(results[section], note)
			}
		}
	}
	data := PageData{
		Title:    "Результаты поиска: " + query,
		Sections: results,
		Query:    query,
		Flash:    getFlash(w, r),
	}
	tmpl := template.Must(template.ParseFiles("templates/index.html"))
	err := tmpl.Execute(w, data)
	if err != nil {
		log.Printf("Ошибка рендеринга шаблона search: %v", err)
		http.Error(w, "Внутренняя ошибка сервера", http.StatusInternalServerError)
	}
}

func deleteSectionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	sectionName := strings.TrimPrefix(r.URL.Path, "/delete-section/")
	if sectionName == "" {
		http.Error(w, "Имя раздела обязательно", http.StatusBadRequest)
		return
	}
	var err error
	sectionName, err = url.QueryUnescape(sectionName)
	if err != nil {
		http.Error(w, "Некорректное имя раздела", http.StatusBadRequest)
		return
	}
	if sectionName == "" || sectionName == "." || sectionName == ".." ||
		strings.Contains(sectionName, "..") {
		http.Error(w, "Недопустимое имя раздела", http.StatusBadRequest)
		return
	}
	if sectionName == "Общие" {
		http.Error(w, "Нельзя удалить раздел 'Общие'", http.StatusForbidden)
		return
	}
	sectionPath := filepath.Join("notes", sectionName)
	if _, err := os.Stat(sectionPath); os.IsNotExist(err) {
		http.Error(w, "Раздел не найден", http.StatusNotFound)
		return
	}
	log.Printf("Удаление раздела: %s", sectionPath)
	err = os.RemoveAll(sectionPath)
	if err != nil {
		log.Printf("Ошибка удаления папки раздела %s: %v", sectionName, err)
		http.Error(w, "Ошибка удаления раздела", http.StatusInternalServerError)
		return
	}
	updateNotesCache()
	log.Printf("Раздел успешно удален: %s", sectionPath)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Раздел успешно удален"))
}

func editSectionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}

	sectionName := strings.TrimPrefix(r.URL.Path, "/edit-section/")
	if sectionName == "" {
		http.NotFound(w, r)
		return
	}

	var err error
	sectionName, err = url.QueryUnescape(sectionName)
	if err != nil {
		http.Error(w, "Некорректное имя раздела", http.StatusBadRequest)
		return
	}

	notes := loadNotesCached()

	if _, exists := notes[sectionName]; !exists && sectionName != "Общие" {
		http.NotFound(w, r)
		return
	}

	currentNotes := notes[sectionName]

	data := SectionEditData{
		Title:       "Редактирование раздела: " + sectionName,
		Section:     sectionName,
		Notes:       currentNotes,
		AllSections: notes,
		Flash:       getFlash(w, r),
	}

	tmpl := template.Must(template.ParseFiles("templates/edit-section.html"))
	err = tmpl.Execute(w, data)
	if err != nil {
		log.Printf("Ошибка рендеринга шаблона edit-section.html: %v", err)
		http.Error(w, "Внутренняя ошибка сервера", http.StatusInternalServerError)
	}
}

func renameSectionHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("INFO: renameSectionHandler called. Method: %s", r.Method)

	if r.Method != http.MethodPost {
		log.Printf("ERROR: Invalid method %s", r.Method)
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}

	log.Println("INFO: Parsing form...")
	err := r.ParseForm()
	if err != nil {
		log.Printf("ERROR: Failed to parse form: %s", err)
		http.Error(w, "Ошибка обработки формы", http.StatusBadRequest)
		return
	}

	log.Printf("DEBUG: Full form data: %+v", r.Form)

	oldSection := r.FormValue("old_section")
	newSection := r.FormValue("new_section")
	log.Printf("DEBUG: Form values - old_section: '%s', new_section: '%s'", oldSection, newSection)

	if oldSection == "" || newSection == "" {
		log.Printf("ERROR: Empty parameters - old: '%s', new: '%s'", oldSection, newSection)
		http.Error(w, "Имена разделов не могут быть пустыми", http.StatusBadRequest)
		return
	}

	if oldSection == "Общие" {
		log.Printf("ERROR: Attempted to rename default section 'Общие'")
		http.Error(w, "Нельзя переименовать раздел 'Общие'", http.StatusForbidden)
		return
	}

	if !isValidFilename(newSection) {
		log.Printf("ERROR: Invalid filename for new section: '%s'", newSection)
		http.Error(w, "Недопустимые символы в названии раздела", http.StatusBadRequest)
		return
	}

	oldPath := filepath.Join("notes", oldSection)
	newPath := filepath.Join("notes", newSection)

	log.Printf("DEBUG: Checking if old path exists: %s", oldPath)
	if _, err := os.Stat(oldPath); os.IsNotExist(err) {
		log.Printf("ERROR: Old section not found: %s", oldPath)
		http.Error(w, "Раздел не найден", http.StatusNotFound)
		return
	}

	log.Printf("DEBUG: Checking if new path already exists: %s", newPath)
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		log.Printf("ERROR: Section already exists: %s", newPath)
		http.Error(w, "Раздел с таким именем уже существует", http.StatusBadRequest)
		return
	}

	log.Printf("INFO: Attempting to rename %s to %s", oldPath, newPath)
	err = os.Rename(oldPath, newPath)
	if err != nil {
		log.Printf("ERROR: Rename failed: %v", err)
		http.Error(w, "Ошибка переименования раздела", http.StatusInternalServerError)
		return
	}

	log.Printf("INFO: Successfully renamed section %s to %s", oldSection, newSection)
	updateNotesCache()
	setFlash(w, "Раздел успешно переименован")

	redirectSection := url.QueryEscape(newSection)
	log.Printf("DEBUG: Redirecting to /edit-section/%s", redirectSection)
	http.Redirect(w, r, "/edit-section/"+redirectSection, http.StatusSeeOther)
}

func moveNoteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}

	err := r.ParseForm()
	if err != nil {
		http.Error(w, "Ошибка обработки формы", http.StatusBadRequest)
		return
	}

	notePath := r.FormValue("note_path")
	newSection := strings.TrimSpace(r.FormValue("new_section"))

	if notePath == "" || newSection == "" {
		http.Error(w, "Не указаны путь заметки или новый раздел", http.StatusBadRequest)
		return
	}

	oldFullPath := filepath.Join("notes", notePath+".txt")
	newSectionPath := filepath.Join("notes", newSection)
	newFullPath := filepath.Join(newSectionPath, filepath.Base(notePath)+".txt")

	if _, err := os.Stat(oldFullPath); os.IsNotExist(err) {
		http.Error(w, "Заметка не найдена", http.StatusNotFound)
		return
	}

	log.Printf("Перемещение заметки: %s -> раздел %s", oldFullPath, newSection)
	if err := os.MkdirAll(newSectionPath, 0755); err != nil {
		log.Printf("Ошибка создания раздела %s: %v", newSectionPath, err)
		http.Error(w, "Ошибка создания раздела", http.StatusInternalServerError)
		return
	}

	if err := os.Rename(oldFullPath, newFullPath); err != nil {
		log.Printf("Ошибка перемещения заметки %s -> %s: %v", oldFullPath, newFullPath, err)
		http.Error(w, "Ошибка перемещения заметки", http.StatusInternalServerError)
		return
	}

	updateNotesCache()
	setFlash(w, "Заметка успешно перемещена")
	log.Printf("Заметка успешно перемещена: %s -> %s", oldFullPath, newFullPath)

	currentSection := filepath.Dir(notePath)
	if currentSection == "." {
		currentSection = "Общие"
	}

	http.Redirect(w, r, "/edit-section/"+url.QueryEscape(currentSection), http.StatusSeeOther)
}

func importExportHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	tmpl := template.Must(template.ParseFiles("templates/import-export.html"))
	err := tmpl.Execute(w, nil)
	if err != nil {
		log.Printf("Ошибка рендеринга import-export.html: %v", err)
		http.Error(w, "Внутренняя ошибка сервера", http.StatusInternalServerError)
	}
}

func categoriesHandler(w http.ResponseWriter, r *http.Request) {
	notes := loadNotesCached()
	categories := make([]string, 0, len(notes))
	for cat := range notes {
		categories = append(categories, cat)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	jsonBytes := []byte("[")
	for i, cat := range categories {
		if i > 0 {
			jsonBytes = append(jsonBytes, ',')
		}
		jsonBytes = append(jsonBytes, []byte(`"`+cat+`"`)...)
	}
	jsonBytes = append(jsonBytes, ']')
	w.Write(jsonBytes)
}

func exportTxtHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("Экспорт всех заметок в TXT")
	notes := loadNotesCached()
	var buffer bytes.Buffer

	for section, notesList := range notes {
		buffer.WriteString(fmt.Sprintf("=== %s ===\n\n", section))
		for _, note := range notesList {
			buffer.WriteString(fmt.Sprintf("Название: %s\n", note.Title))
			buffer.WriteString(fmt.Sprintf("Содержимое:\n%s\n", note.Content))
			buffer.WriteString("\n---\n\n")
		}
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=notes.txt")
	w.Write(buffer.Bytes())
	log.Println("Экспорт TXT завершён")
}

func exportDocHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("Экспорт всех заметок в DOC")
	notes := loadNotesCached()
	var rtfBuffer bytes.Buffer

	rtfBuffer.WriteString("{\\rtf1\\ansi\\ansicpg1252\\uc1\\deff0 {\\fonttbl {\\f0 Times New Roman;}}\\f0\\fs24\n")

	rtfEncode := func(s string) string {
		var out strings.Builder
		for _, r := range s {
			if r < 0x80 && r != '\\' && r != '{' && r != '}' {
				out.WriteRune(r)
			} else {
				if r == '\\' || r == '{' || r == '}' {
					out.WriteByte('\\')
					out.WriteRune(r)
				} else {
					out.WriteString(fmt.Sprintf("\\u%d?", r))
				}
			}
		}
		return out.String()
	}

	for section, notesList := range notes {
		sectionTitle := rtfEncode(fmt.Sprintf("=== %s ===", section))
		rtfBuffer.WriteString(fmt.Sprintf("\\b %s \\b0\\par\\par\n", sectionTitle))

		for _, note := range notesList {
			title := rtfEncode(fmt.Sprintf("Название: %s", note.Title))
			rtfBuffer.WriteString(fmt.Sprintf("\\b %s \\b0\\par\n", title))

			content := rtfEncode(note.Content)
			content = strings.ReplaceAll(content, "\r\n", "\n")
			content = strings.ReplaceAll(content, "\n", "\\par\n")
			rtfBuffer.WriteString(content)
			rtfBuffer.WriteString("\\par\\par\n")

			rtfBuffer.WriteString("---\\par\\par\n")
		}
	}
	rtfBuffer.WriteString("}")

	w.Header().Set("Content-Type", "application/msword")
	w.Header().Set("Content-Disposition", "attachment; filename=notes.doc")
	w.Write(rtfBuffer.Bytes())
	log.Println("Экспорт DOC завершён")
}

func importHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}

	err := r.ParseMultipartForm(10 << 20)
	if err != nil {
		log.Printf("Ошибка парсинга multipart при импорте: %v", err)
		http.Error(w, "Ошибка обработки файла", http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		log.Printf("Ошибка получения файла при импорте: %v", err)
		http.Error(w, "Файл не передан", http.StatusBadRequest)
		return
	}
	defer file.Close()

	category := strings.TrimSpace(r.FormValue("category"))
	title := strings.TrimSpace(r.FormValue("title"))

	if category == "" || title == "" {
		http.Error(w, "Категория и название обязательны", http.StatusBadRequest)
		return
	}

	if !isValidFilename(category) || !isValidFilename(title) {
		http.Error(w, "Недопустимые символы в названии категории или заметки", http.StatusBadRequest)
		return
	}

	fileBytes, err := io.ReadAll(file)
	if err != nil {
		log.Printf("Ошибка чтения файла при импорте: %v", err)
		http.Error(w, "Ошибка чтения файла", http.StatusInternalServerError)
		return
	}
	content := string(fileBytes)

	ext := strings.ToLower(filepath.Ext(header.Filename))
	if ext != ".txt" && ext != ".doc" {
		http.Error(w, "Поддерживаются только файлы .txt и .doc", http.StatusBadRequest)
		return
	}

	category = strings.Trim(category, "/\\")
	if category == "." {
		category = "Общие"
	}
	categoryPath := filepath.Join("notes", category)
	err = os.MkdirAll(categoryPath, 0755)
	if err != nil {
		log.Printf("Ошибка создания папки категории %s: %v", categoryPath, err)
		http.Error(w, "Ошибка создания категории", http.StatusInternalServerError)
		return
	}

	fullPath := filepath.Join(categoryPath, title+".txt")
	if _, err := os.Stat(fullPath); err == nil {
		http.Error(w, "Заметка с таким названием уже существует", http.StatusBadRequest)
		return
	}

	log.Printf("Импорт заметки: категория=%s, название=%s, файл=%s", category, title, header.Filename)
	err = os.WriteFile(fullPath, []byte(content), 0644)
	if err != nil {
		log.Printf("Ошибка сохранения файла при импорте %s: %v", fullPath, err)
		http.Error(w, "Ошибка сохранения заметки", http.StatusInternalServerError)
		return
	}

	updateNotesCache()
	log.Printf("Заметка успешно импортирована: %s", fullPath)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"message":"Заметка успешно импортирована"}`))
}
