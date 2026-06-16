package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
)

type bwMode int

const (
	bwNormal bwMode = iota
	bwLight
	bwDark
)

var (
	adminIDs  = make(map[int64]bool)
	userModes = make(map[int64]bwMode)
	modesMu   sync.RWMutex
	httpCli   *http.Client
	db        *sql.DB
)

func main() {
	godotenv.Load()

	token := os.Getenv("token")

	// Parse admin IDs from env (comma-separated)
	for _, idStr := range strings.Split(os.Getenv("admin_ids"), ",") {
		idStr = strings.TrimSpace(idStr)
		if id, err := strconv.ParseInt(idStr, 10, 64); err == nil && id > 0 {
			adminIDs[id] = true
		}
	}

	initDB()

	transport := &http.Transport{}
	if proxyURL := os.Getenv("proxy"); proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err == nil {
			transport.Proxy = http.ProxyURL(u)
		}
	}
	httpCli = &http.Client{Transport: transport}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	opts := []bot.Option{
		bot.WithDefaultHandler(handler),
		bot.WithHTTPClient(30*time.Second, httpCli),
		bot.WithAllowedUpdates([]string{"message", "edited_message", "callback_query", "my_chat_member", "inline_query"}),
	}

	b, err := bot.New(token, opts...)
	if err != nil {
		panic(err)
	}

	b.SetMyCommands(ctx, &bot.SetMyCommandsParams{
		Commands: []models.BotCommand{
			{Command: "start", Description: "Запуск бота (ЛС)"},
			{Command: "help", Description: "Помощь"},
			{Command: "bw", Description: "Обычный ЧБ (в группе)"},
			{Command: "bwl", Description: "Светлый ЧБ (в группе)"},
			{Command: "bwd", Description: "Тёмный ЧБ (в группе)"},
		},
		Scope: &models.BotCommandScopeDefault{},
	})

	b.Start(ctx)
}

func isAdmin(id int64) bool {
	return adminIDs[id]
}

func initDB() {
	var err error
	db, err = sql.Open("sqlite3", "bot_data.db?_journal_mode=WAL")
	if err != nil {
		panic(err)
	}

	db.Exec(`CREATE TABLE IF NOT EXISTS events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL,
		username TEXT,
		first_name TEXT,
		last_name TEXT,
		chat_id INTEGER NOT NULL,
		chat_type TEXT,
		message_id INTEGER,
		file_id TEXT,
		event_type TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_events_user ON events(user_id)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_events_created ON events(created_at)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_events_photo ON events(event_type, created_at)`)

	var colCount int
	db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('events') WHERE name='file_id'`).Scan(&colCount)
	if colCount == 0 {
		db.Exec(`ALTER TABLE events ADD COLUMN file_id TEXT`)
	}
}

func logEvent(userID int64, username, firstName, lastName, chatType string, chatID int64, messageID int, eventType, fileID string) {
	db.Exec(`INSERT INTO events (user_id, username, first_name, last_name, chat_id, chat_type, message_id, file_id, event_type) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		userID, username, firstName, lastName, chatID, chatType, messageID, fileID, eventType)
}

func getMode(userID int64) bwMode {
	modesMu.RLock()
	defer modesMu.RUnlock()
	if m, ok := userModes[userID]; ok {
		return m
	}
	return bwNormal
}

func setMode(userID int64, m bwMode) {
	modesMu.Lock()
	defer modesMu.Unlock()
	userModes[userID] = m
}

func modeName(m bwMode) string {
	switch m {
	case bwLight:
		return "Светлый"
	case bwDark:
		return "Тёмный"
	default:
		return "Обычный"
	}
}

func modeEmoji(m bwMode) string {
	switch m {
	case bwLight:
		return "☀️"
	case bwDark:
		return "🌑"
	default:
		return "🔘"
	}
}

func queryInt(query string, args ...any) int {
	var val int
	db.QueryRow(query, args...).Scan(&val)
	return val
}

func fmtTime(s string) string {
	if len(s) >= 16 {
		return s[5:16]
	}
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

// ──────────────────── KEYBOARDS ────────────────────

func replyKeyboard(userID int64) models.ReplyKeyboardMarkup {
	rows := [][]models.KeyboardButton{
		{
			{Text: "🔘 Обычный"},
			{Text: "☀️ Светлый"},
			{Text: "🌑 Тёмный"},
		},
	}

	if isAdmin(userID) {
		rows = append(rows, []models.KeyboardButton{
			{Text: "📊 Статистика"},
			{Text: "📸 Фото"},
			{Text: "👥 Юзеры"},
			{Text: "📋 Логи"},
		})
	}

	return models.ReplyKeyboardMarkup{
		Keyboard:       rows,
		ResizeKeyboard: true,
	}
}

func statusText(userID int64) string {
	mode := getMode(userID)
	return fmt.Sprintf("Режим: %s %s\nОтправь фото в любой чат с ботом.", modeEmoji(mode), modeName(mode))
}

// ──────────────────── HANDLER ────────────────────

func handler(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message != nil {
		handleMessage(ctx, b, update.Message)
		return
	}
	if update.CallbackQuery != nil {
		handleCallback(ctx, b, update.CallbackQuery)
		return
	}
	if update.InlineQuery != nil {
		handleInlineQuery(ctx, b, update.InlineQuery)
		return
	}
	if update.MyChatMember != nil {
		t := update.MyChatMember.NewChatMember.Type
		if t == "member" || t == "administrator" || t == "creator" {
			b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: update.MyChatMember.Chat.ID,
				Text: "Привет! Я конвертирую фото в чёрно-белое.\n\n" +
					"Команды:\n" +
					"/bw — обычный ЧБ\n" +
					"/bwl — светлый ЧБ\n" +
					"/bwd — тёмный ЧБ\n\n" +
					"⚠️ Дайте мне права админа чтобы я видел фото!\n\n" +
					"Также работ Inline Mode — наберите @dobropic_bot в любом чате.",
			})
		}
	}
}

func handleMessage(ctx context.Context, b *bot.Bot, msg *models.Message) {
	user := msg.From
	text := msg.Text

	// Strip @bot mention in groups: "@dobropic_bot /bw" -> "/bw"
	if msg.Chat.Type != "private" && strings.HasPrefix(text, "@") {
		if idx := strings.Index(text, " "); idx > 0 {
			text = strings.TrimSpace(text[idx:])
		}
	}

	var fileID string
	if len(msg.Photo) > 0 {
		fileID = msg.Photo[len(msg.Photo)-1].FileID
	}
	logEvent(user.ID, user.Username, user.FirstName, user.LastName, string(msg.Chat.Type), msg.Chat.ID, msg.ID, "message", fileID)

	// Group commands: /bw /bwl /bwd
	if msg.Chat.Type != "private" {
		switch {
		case text == "/bw" || text == "/bwl" || text == "/bwd":
			if isAdmin(user.ID) {
				switch text {
				case "/bw":
					setMode(user.ID, bwNormal)
					sendReply(ctx, b, msg, "Режим: 🔘 Обычный")
				case "/bwl":
					setMode(user.ID, bwLight)
					sendReply(ctx, b, msg, "Режим: ☀️ Светлый")
				case "/bwd":
					setMode(user.ID, bwDark)
					sendReply(ctx, b, msg, "Режим: 🌑 Тёмный")
				}
				return
			}
			// Non-admin: reply only in group, visible only to user (via reply)
			switch text {
			case "/bw":
				setMode(user.ID, bwNormal)
				sendReply(ctx, b, msg, fmt.Sprintf("%s: 🔘 Обычный", user.FirstName))
			case "/bwl":
				setMode(user.ID, bwLight)
				sendReply(ctx, b, msg, fmt.Sprintf("%s: ☀️ Светлый", user.FirstName))
			case "/bwd":
				setMode(user.ID, bwDark)
				sendReply(ctx, b, msg, fmt.Sprintf("%s: 🌑 Тёмный", user.FirstName))
			}
			return
		}
	}

	// Photo processing in groups
	if msg.Photo != nil && msg.Chat.Type != "private" {
		processPhoto(ctx, b, msg)
		return
	}

	// Private chat handling
	if msg.Chat.Type == "private" {
		handlePrivateMessage(ctx, b, msg, text)
	}
}

func handlePrivateMessage(ctx context.Context, b *bot.Bot, msg *models.Message, text string) {
	user := msg.From

	// Admin commands with args
	if isAdmin(user.ID) {
		if strings.HasPrefix(text, "/view ") {
			handleViewPhoto(ctx, b, msg)
			return
		}
		if strings.HasPrefix(text, "/ban ") {
			handleBan(ctx, b, msg)
			return
		}
		if strings.HasPrefix(text, "/cleanup ") {
			handleCleanup(ctx, b, msg)
			return
		}
		if strings.HasPrefix(text, "/userphotos ") {
			handleUserPhotos(ctx, b, msg)
			return
		}
	}

	// ReplyKeyboard button presses
	switch text {
	case "🔘 Обычный":
		setMode(user.ID, bwNormal)
		sendReply(ctx, b, msg, "Режим: 🔘 Обычный")
		return
	case "☀️ Светлый":
		setMode(user.ID, bwLight)
		sendReply(ctx, b, msg, "Режим: ☀️ Светлый")
		return
	case "🌑 Тёмный":
		setMode(user.ID, bwDark)
		sendReply(ctx, b, msg, "Режим: 🌑 Тёмный")
		return
	case "📊 Статистика":
		if !isAdmin(user.ID) {
			return
		}
		showAdminStats(ctx, b, msg)
		return
	case "📸 Фото":
		if !isAdmin(user.ID) {
			return
		}
		showAdminPhotos(ctx, b, msg)
		return
	case "👥 Юзеры":
		if !isAdmin(user.ID) {
			return
		}
		showAdminUsers(ctx, b, msg)
		return
	case "📋 Логи":
		if !isAdmin(user.ID) {
			return
		}
		showAdminLog(ctx, b, msg)
		return
	case "🔍 Юзер":
		if !isAdmin(user.ID) {
			return
		}
		sendReply(ctx, b, msg, "Введите: /userphotos 123456\nили: /userphotos @username")
		return
	}

	// Commands
	switch text {
	case "/start":
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:      msg.Chat.ID,
			Text:        statusText(user.ID),
			ReplyMarkup: replyKeyboard(user.ID),
		})
		return
	case "/help":
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: msg.Chat.ID,
			Text: "📸 Отправь фото в любой чат с ботом — получи ЧБ версию.\n\n" +
				"Команды в группах:\n" +
				"/bw — обычный ЧБ\n" +
				"/bwl — светлый ЧБ\n" +
				"/bwd — тёмный ЧБ\n\n" +
				"Inline mode (без добавления в группу):\n" +
				"@dobropic_bot — последние фото\n" +
				"@dobropic_bot @username — фото пользователя\n" +
				"@dobropic_bot 123 456 — фото по ID",
			ReplyMarkup: replyKeyboard(user.ID),
		})
		return
	case "/reset":
		setMode(user.ID, bwNormal)
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:      msg.Chat.ID,
			Text:        "Режим сброшен на обычный.",
			ReplyMarkup: replyKeyboard(user.ID),
		})
		return
	case "/stats":
		if !isAdmin(user.ID) {
			return
		}
		showAdminStats(ctx, b, msg)
		return
	case "/users":
		if !isAdmin(user.ID) {
			return
		}
		showAdminUsers(ctx, b, msg)
		return
	case "/log":
		if !isAdmin(user.ID) {
			return
		}
		showAdminLog(ctx, b, msg)
		return
	case "/photos":
		if !isAdmin(user.ID) {
			return
		}
		showAdminPhotos(ctx, b, msg)
		return
	}

	if msg.Photo != nil {
		processPhoto(ctx, b, msg)
	}
}

func sendReply(ctx context.Context, b *bot.Bot, msg *models.Message, text string) {
	b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: msg.Chat.ID,
		Text:   text,
		ReplyParameters: &models.ReplyParameters{
			MessageID: msg.ID,
		},
	})
}

// ──────────────────── CALLBACKS ────────────────────

func handleCallback(ctx context.Context, b *bot.Bot, cb *models.CallbackQuery) {
	data := cb.Data

	if strings.HasPrefix(data, "uphotos_") {
		uid, _ := strconv.ParseInt(strings.TrimPrefix(data, "uphotos_"), 10, 64)
		sendUserPhotosList(ctx, b, cb, uid)
		return
	}
	if strings.HasPrefix(data, "uban_") {
		uid, _ := strconv.ParseInt(strings.TrimPrefix(data, "uban_"), 10, 64)
		banByID(ctx, b, cb, uid)
		return
	}
	if strings.HasPrefix(data, "viewphoto_") {
		eid, _ := strconv.Atoi(strings.TrimPrefix(data, "viewphoto_"))
		sendPhotoByEventID(ctx, b, cb, eid)
		return
	}
}

func sendUserPhotosList(ctx context.Context, b *bot.Bot, cb *models.CallbackQuery, targetID int64) {
	if !isAdmin(cb.From.ID) {
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cb.ID, Text: "Нет доступа", ShowAlert: true})
		return
	}

	var uname, firstName, lastName sql.NullString
	db.QueryRow(`SELECT username, first_name, last_name FROM events WHERE user_id=? LIMIT 1`, targetID).Scan(&uname, &firstName, &lastName)

	name := ""
	if firstName.Valid {
		name = firstName.String
	}
	if lastName.Valid {
		name += " " + lastName.String
	}
	if name == "" {
		name = fmt.Sprintf("ID %d", targetID)
	}
	unameStr := ""
	if uname.Valid {
		unameStr = " @" + uname.String
	}

	rows, err := db.Query(`SELECT id, file_id, chat_id, created_at
		FROM events WHERE user_id=? AND event_type='photo'
		ORDER BY id DESC`, targetID)
	if err != nil {
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cb.ID, Text: "Ошибка", ShowAlert: true})
		return
	}
	defer rows.Close()

	type entry struct {
		id   int
		time string
		chat int64
	}
	var photos []entry
	for rows.Next() {
		var e entry
		var fileID string
		rows.Scan(&e.id, &fileID, &e.chat, &e.time)
		photos = append(photos, e)
	}

	if len(photos) == 0 {
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cb.ID, Text: "Нет фото", ShowAlert: true})
		return
	}

	text := fmt.Sprintf("📸 Фото от %s%s (ID: %d) — %d шт.\n\n", name, unameStr, targetID, len(photos))

	keyboard := [][]models.InlineKeyboardButton{}
	limit := len(photos)
	if limit > 15 {
		limit = 15
	}

	for i := 0; i < limit; i++ {
		p := photos[i]
		keyboard = append(keyboard, []models.InlineKeyboardButton{
			{Text: fmt.Sprintf("#%d %s chat:%d", p.id, fmtTime(p.time), p.chat), CallbackData: fmt.Sprintf("viewphoto_%d", p.id)},
		})
	}

	if len(photos) > 15 {
		text += "Показаны последние 15.\n"
	}

	text += "\nНажми кнопку чтобы посмотреть фото."

	b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cb.ID})

	b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      cb.Message.Message.Chat.ID,
		Text:        text,
		ReplyMarkup: models.InlineKeyboardMarkup{InlineKeyboard: keyboard},
	})
}

func banByID(ctx context.Context, b *bot.Bot, cb *models.CallbackQuery, targetID int64) {
	if !isAdmin(cb.From.ID) {
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cb.ID, Text: "Нет доступа", ShowAlert: true})
		return
	}

	var chatID int64
	err := db.QueryRow(`SELECT chat_id FROM events WHERE user_id=? AND chat_type != 'private' ORDER BY id DESC LIMIT 1`, targetID).Scan(&chatID)
	if err != nil {
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cb.ID, Text: "Юзер не найден в группах", ShowAlert: true})
		return
	}

	_, err = b.BanChatMember(ctx, &bot.BanChatMemberParams{ChatID: chatID, UserID: targetID})
	if err != nil {
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cb.ID, Text: fmt.Sprintf("Ошибка: %v", err), ShowAlert: true})
		return
	}

	b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cb.ID, Text: fmt.Sprintf("✅ %d забанен в %d", targetID, chatID), ShowAlert: true})
}

func sendPhotoByEventID(ctx context.Context, b *bot.Bot, cb *models.CallbackQuery, eventID int) {
	if !isAdmin(cb.From.ID) {
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cb.ID, Text: "Нет доступа", ShowAlert: true})
		return
	}

	var fileID string
	var uid int64
	var uname, firstName sql.NullString
	var chatID int64
	var createdAt string

	err := db.QueryRow(`SELECT file_id, user_id, username, first_name, chat_id, created_at
		FROM events WHERE id=? AND event_type='photo'`, eventID).Scan(&fileID, &uid, &uname, &firstName, &chatID, &createdAt)
	if err != nil {
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cb.ID, Text: "Не найдено", ShowAlert: true})
		return
	}

	if fileID == "" {
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cb.ID, Text: "Нет file_id", ShowAlert: true})
		return
	}

	name := ""
	if firstName.Valid {
		name = firstName.String
	}
	if uname.Valid {
		name += " @" + uname.String
	}

	caption := fmt.Sprintf("#%d | %s | %s | chat %d", eventID, fmtTime(createdAt), name, chatID)

	b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cb.ID})

	b.SendPhoto(ctx, &bot.SendPhotoParams{
		ChatID:  cb.Message.Message.Chat.ID,
		Photo:   &models.InputFileString{Data: fileID},
		Caption: caption,
	})
}

// ──────────────────── INLINE MODE ────────────────────

func handleInlineQuery(ctx context.Context, b *bot.Bot, iq *models.InlineQuery) {
	query := strings.TrimSpace(iq.Query)
	var results []models.InlineQueryResult

	if query == "" {
		// Empty query → show last 20 photos
		rows, err := db.Query(`SELECT id, file_id, user_id, username, first_name, chat_id, created_at
			FROM events WHERE event_type='photo' AND file_id != ''
			ORDER BY id DESC LIMIT 20`)
		if err != nil {
			b.AnswerInlineQuery(ctx, &bot.AnswerInlineQueryParams{
				InlineQueryID: iq.ID,
				Results:       []models.InlineQueryResult{},
			})
			return
		}
		defer rows.Close()

		for rows.Next() {
			var eventID int
			var fileID string
			var uid int64
			var uname, firstName sql.NullString
			var chatID int64
			var createdAt string
			rows.Scan(&eventID, &fileID, &uid, &uname, &firstName, &chatID, &createdAt)

			name := fmtName(firstName, uname, uid)
			caption := fmt.Sprintf("#%d | %s | %s | chat %d", eventID, fmtTime(createdAt), name, chatID)

			results = append(results, &models.InlineQueryResultCachedPhoto{
				ID:          strconv.Itoa(eventID),
				PhotoFileID: fileID,
				Title:       fmt.Sprintf("Фото #%d", eventID),
				Description: fmt.Sprintf("%s — %s", name, fmtTime(createdAt)),
				Caption:     caption,
			})
		}
	} else if strings.HasPrefix(query, "@") {
		// @username → photos from that user
		username := strings.TrimPrefix(query, "@")
		var targetID int64
		err := db.QueryRow(`SELECT user_id FROM events WHERE username=? LIMIT 1`, username).Scan(&targetID)
		if err != nil {
			b.AnswerInlineQuery(ctx, &bot.AnswerInlineQueryParams{
				InlineQueryID: iq.ID,
				Results:       []models.InlineQueryResult{},
			})
			return
		}

		rows, err := db.Query(`SELECT id, file_id, user_id, username, first_name, chat_id, created_at
			FROM events WHERE user_id=? AND event_type='photo' AND file_id != ''
			ORDER BY id DESC LIMIT 20`, targetID)
		if err != nil {
			b.AnswerInlineQuery(ctx, &bot.AnswerInlineQueryParams{
				InlineQueryID: iq.ID,
				Results:       []models.InlineQueryResult{},
			})
			return
		}
		defer rows.Close()

		for rows.Next() {
			var eventID int
			var fileID string
			var uid int64
			var uname, firstName sql.NullString
			var chatID int64
			var createdAt string
			rows.Scan(&eventID, &fileID, &uid, &uname, &firstName, &chatID, &createdAt)

			name := fmtName(firstName, uname, uid)
			caption := fmt.Sprintf("#%d | %s | %s | chat %d", eventID, fmtTime(createdAt), name, chatID)

			results = append(results, &models.InlineQueryResultCachedPhoto{
				ID:          strconv.Itoa(eventID),
				PhotoFileID: fileID,
				Title:       fmt.Sprintf("Фото #%d", eventID),
				Description: fmt.Sprintf("%s — %s", name, fmtTime(createdAt)),
				Caption:     caption,
			})
		}
	} else {
		// Try as comma-separated IDs: "10 16 24" or "10,16,24"
		ids := strings.FieldsFunc(query, func(r rune) bool { return r == ' ' || r == ',' || r == ';' })

		for _, idStr := range ids {
			eventID, err := strconv.Atoi(idStr)
			if err != nil {
				continue
			}

			var fileID string
			var uid int64
			var uname, firstName sql.NullString
			var chatID int64
			var createdAt string

			err = db.QueryRow(`SELECT file_id, user_id, username, first_name, chat_id, created_at
				FROM events WHERE id=? AND event_type='photo' AND file_id != ''`, eventID).Scan(
				&fileID, &uid, &uname, &firstName, &chatID, &createdAt)
			if err != nil {
				continue
			}

			name := fmtName(firstName, uname, uid)
			caption := fmt.Sprintf("#%d | %s | %s | chat %d", eventID, fmtTime(createdAt), name, chatID)

			results = append(results, &models.InlineQueryResultCachedPhoto{
				ID:          strconv.Itoa(eventID),
				PhotoFileID: fileID,
				Title:       fmt.Sprintf("Фото #%d", eventID),
				Description: fmt.Sprintf("%s — %s", name, fmtTime(createdAt)),
				Caption:     caption,
			})
		}
	}

	if len(results) == 0 {
		results = []models.InlineQueryResult{}
	}

	b.AnswerInlineQuery(ctx, &bot.AnswerInlineQueryParams{
		InlineQueryID: iq.ID,
		Results:       results,
		CacheTime:     60,
		IsPersonal:    true,
	})
}

func fmtName(firstName, username sql.NullString, uid int64) string {
	name := ""
	if firstName.Valid {
		name = firstName.String
	}
	if username.Valid {
		name += " @" + username.String
	}
	if name == "" {
		name = fmt.Sprintf("ID %d", uid)
	}
	return name
}

// ──────────────────── PHOTO PROCESSING ────────────────────

func processPhoto(ctx context.Context, b *bot.Bot, msg *models.Message) {
	photo := msg.Photo[len(msg.Photo)-1]

	file, err := b.GetFile(ctx, &bot.GetFileParams{FileID: photo.FileID})
	if err != nil {
		return
	}

	imgData, err := downloadFile(file.FilePath)
	if err != nil {
		return
	}

	mode := getMode(msg.From.ID)
	logEvent(msg.From.ID, msg.From.Username, msg.From.FirstName, msg.From.LastName, string(msg.Chat.Type), msg.Chat.ID, msg.ID, "photo", photo.FileID)

	img, imgType, err := decodeImage(imgData)
	if err != nil {
		return
	}

	bwImg := toGrayscale(img, mode)

	var buf bytes.Buffer
	switch imgType {
	case "png":
		png.Encode(&buf, bwImg)
	default:
		jpeg.Encode(&buf, bwImg, &jpeg.Options{Quality: 95})
	}

	b.SendPhoto(ctx, &bot.SendPhotoParams{
		ChatID: msg.Chat.ID,
		Photo:  &models.InputFileUpload{Data: bytes.NewReader(buf.Bytes())},
		ReplyParameters: &models.ReplyParameters{
			MessageID: msg.ID,
		},
	})
}

func downloadFile(path string) ([]byte, error) {
	u := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", os.Getenv("token"), path)
	resp, err := httpCli.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var buf bytes.Buffer
	_, err = buf.ReadFrom(resp.Body)
	return buf.Bytes(), err
}

func decodeImage(data []byte) (image.Image, string, error) {
	r := bytes.NewReader(data)
	if img, err := png.Decode(r); err == nil {
		return img, "png", nil
	}
	r.Seek(0, 0)
	if img, err := jpeg.Decode(r); err == nil {
		return img, "jpg", nil
	}
	return nil, "", fmt.Errorf("unsupported format")
}

func luminance(r, g, b float64) float64 {
	return r*0.2126 + g*0.7152 + b*0.0722
}

func sigmoid(x, steepness, midpoint float64) float64 {
	return 1.0 / (1.0 + math.Exp(-steepness*(x-midpoint)))
}

func toGrayscale(src image.Image, mode bwMode) image.Image {
	bounds := src.Bounds()
	dst := image.NewRGBA(bounds)
	draw.Draw(dst, bounds, &image.Uniform{color.Black}, image.Point{}, draw.Src)

	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := src.At(x, y).RGBA()
			norm := luminance(float64(r), float64(g), float64(b)) / 65535.0

			var val float64
			switch mode {
			case bwNormal:
				val = norm
			case bwLight:
				v := sigmoid(norm, 8, 0.45)
				minV := sigmoid(0, 8, 0.45)
				maxV := sigmoid(1, 8, 0.45)
				val = (v - minV) / (maxV - minV)
				val = math.Pow(val, 0.75)
			case bwDark:
				v := sigmoid(norm, 8, 0.55)
				minV := sigmoid(0, 8, 0.55)
				maxV := sigmoid(1, 8, 0.55)
				val = (v - minV) / (maxV - minV)
				val = math.Pow(val, 1.3)
			}

			if val < 0 {
				val = 0
			} else if val > 1 {
				val = 1
			}

			dst.Set(x, y, color.Gray{Y: uint8(val * 255.0)})
		}
	}

	return dst
}

// ──────────────────── ADMIN ────────────────────

func showAdminStats(ctx context.Context, b *bot.Bot, msg *models.Message) {
	totalEvents := queryInt("SELECT COUNT(*) FROM events")
	totalPhotos := queryInt("SELECT COUNT(*) FROM events WHERE event_type='photo'")
	totalUsers := queryInt("SELECT COUNT(DISTINCT user_id) FROM events")
	todayPhotos := queryInt("SELECT COUNT(*) FROM events WHERE event_type='photo' AND date(created_at)=date('now')")
	chatsUsed := queryInt("SELECT COUNT(DISTINCT chat_id) FROM events")

	text := fmt.Sprintf(`📊 Статистика

Событий: %d
📸 Фото: %d
👥 Юзеров: %d
💬 Чатов: %d
📸 Сегодня: %d`, totalEvents, totalPhotos, totalUsers, chatsUsed, todayPhotos)

	b.SendMessage(ctx, &bot.SendMessageParams{ChatID: msg.Chat.ID, Text: text, ReplyParameters: &models.ReplyParameters{MessageID: msg.ID}})
}

func showAdminUsers(ctx context.Context, b *bot.Bot, msg *models.Message) {
	rows, err := db.Query(`SELECT user_id, username, first_name, last_name, COUNT(*) as cnt, MAX(created_at) as last_seen
		FROM events GROUP BY user_id ORDER BY last_seen DESC`)
	if err != nil {
		return
	}
	defer rows.Close()

	text := "👥 Пользователи:\n\n"

	keyboard := [][]models.InlineKeyboardButton{}

	for rows.Next() {
		var uid int64
		var username, firstName, lastName sql.NullString
		var cnt int
		var lastSeen string

		rows.Scan(&uid, &username, &firstName, &lastName, &cnt, &lastSeen)

		name := ""
		if firstName.Valid {
			name = firstName.String
		}
		if lastName.Valid {
			name += " " + lastName.String
		}
		if name == "" {
			name = "(нет имени)"
		}

		uname := ""
		if username.Valid {
			uname = " @" + username.String
		}

		text += fmt.Sprintf("%d | %s%s | %d | %s\n", uid, name, uname, cnt, fmtTime(lastSeen))

		keyboard = append(keyboard, []models.InlineKeyboardButton{
			{Text: fmt.Sprintf("📸 %s", name), CallbackData: fmt.Sprintf("uphotos_%d", uid)},
			{Text: "🚫 Бан", CallbackData: fmt.Sprintf("uban_%d", uid)},
		})
	}

	b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      msg.Chat.ID,
		Text:        text,
		ReplyMarkup: models.InlineKeyboardMarkup{InlineKeyboard: keyboard},
		ReplyParameters: &models.ReplyParameters{MessageID: msg.ID},
	})
}

func showAdminLog(ctx context.Context, b *bot.Bot, msg *models.Message) {
	rows, err := db.Query(`SELECT user_id, username, first_name, event_type, chat_type, created_at
		FROM events ORDER BY id DESC LIMIT 30`)
	if err != nil {
		return
	}
	defer rows.Close()

	text := "📋 События (30):\n\n"

	for rows.Next() {
		var uid int64
		var username, firstName sql.NullString
		var eventType, chatType, createdAt string

		rows.Scan(&uid, &username, &firstName, &eventType, &chatType, &createdAt)

		name := ""
		if firstName.Valid {
			name = firstName.String
		}
		if username.Valid {
			name += " @" + username.String
		}
		if name == "" {
			name = fmt.Sprintf("ID %d", uid)
		}

		text += fmt.Sprintf("%s [%s] %s — %s\n", fmtTime(createdAt), chatType, name, eventType)
	}

	b.SendMessage(ctx, &bot.SendMessageParams{ChatID: msg.Chat.ID, Text: text, ReplyParameters: &models.ReplyParameters{MessageID: msg.ID}})
}

func showAdminPhotos(ctx context.Context, b *bot.Bot, msg *models.Message) {
	rows, err := db.Query(`SELECT id, user_id, username, first_name, chat_id, created_at
		FROM events WHERE event_type='photo'
		ORDER BY id DESC LIMIT 20`)
	if err != nil {
		return
	}
	defer rows.Close()

	text := "📸 Последние фото:\n\n"
	keyboard := [][]models.InlineKeyboardButton{}

	for rows.Next() {
		var eventID int
		var uid int64
		var username, firstName sql.NullString
		var chatID int64
		var createdAt string

		rows.Scan(&eventID, &uid, &username, &firstName, &chatID, &createdAt)

		name := ""
		if firstName.Valid {
			name = firstName.String
		}
		if username.Valid {
			name += " @" + username.String
		}
		if name == "" {
			name = fmt.Sprintf("ID %d", uid)
		}

		text += fmt.Sprintf("#%d | %s | %s | chat %d\n", eventID, fmtTime(createdAt), name, chatID)

		keyboard = append(keyboard, []models.InlineKeyboardButton{
			{Text: fmt.Sprintf("🔍 #%d", eventID), CallbackData: fmt.Sprintf("viewphoto_%d", eventID)},
		})
	}

	text += "\nНажми кнопку чтобы посмотреть."

	b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      msg.Chat.ID,
		Text:        text,
		ReplyMarkup: models.InlineKeyboardMarkup{InlineKeyboard: keyboard},
		ReplyParameters: &models.ReplyParameters{MessageID: msg.ID},
	})
}

func handleViewPhoto(ctx context.Context, b *bot.Bot, msg *models.Message) {
	parts := strings.Fields(msg.Text)
	if len(parts) < 2 {
		sendReply(ctx, b, msg, "Использование: /view 123\nили: /view 123 456 789")
		return
	}

	for _, part := range parts[1:] {
		eventID, err := strconv.Atoi(part)
		if err != nil {
			sendReply(ctx, b, msg, fmt.Sprintf("«%s» — не число, пропущено.", part))
			continue
		}

		var fileID string
		var uid int64
		var uname, firstName sql.NullString
		var chatID int64
		var createdAt string

		err = db.QueryRow(`SELECT file_id, user_id, username, first_name, chat_id, created_at
			FROM events WHERE id=? AND event_type='photo'`, eventID).Scan(&fileID, &uid, &uname, &firstName, &chatID, &createdAt)
		if err != nil {
			sendReply(ctx, b, msg, fmt.Sprintf("#%d не найдено.", eventID))
			continue
		}

		if fileID == "" {
			sendReply(ctx, b, msg, fmt.Sprintf("#%d — нет file_id.", eventID))
			continue
		}

		name := ""
		if firstName.Valid {
			name = firstName.String
		}
		if uname.Valid {
			name += " @" + uname.String
		}

		caption := fmt.Sprintf("#%d | %s | %s | chat %d", eventID, fmtTime(createdAt), name, chatID)

		b.SendPhoto(ctx, &bot.SendPhotoParams{
			ChatID:  msg.Chat.ID,
			Photo:   &models.InputFileString{Data: fileID},
			Caption: caption,
			ReplyParameters: &models.ReplyParameters{
				MessageID: msg.ID,
			},
		})
	}
}

func handleUserPhotos(ctx context.Context, b *bot.Bot, msg *models.Message) {
	parts := strings.Fields(msg.Text)
	if len(parts) < 2 {
		sendReply(ctx, b, msg, "Использование: /userphotos 123456\nили: /userphotos @username")
		return
	}

	target := parts[1]
	var targetID int64

	if strings.HasPrefix(target, "@") {
		username := strings.TrimPrefix(target, "@")
		err := db.QueryRow(`SELECT user_id FROM events WHERE username=? LIMIT 1`, username).Scan(&targetID)
		if err != nil {
			sendReply(ctx, b, msg, fmt.Sprintf("Пользователь %s не найден.", target))
			return
		}
	} else {
		id, err := strconv.ParseInt(target, 10, 64)
		if err != nil {
			sendReply(ctx, b, msg, "Неверный ID.")
			return
		}
		targetID = id
	}

	var uname, firstName, lastName sql.NullString
	db.QueryRow(`SELECT username, first_name, last_name FROM events WHERE user_id=? LIMIT 1`, targetID).Scan(&uname, &firstName, &lastName)

	name := ""
	if firstName.Valid {
		name = firstName.String
	}
	if lastName.Valid {
		name += " " + lastName.String
	}
	if name == "" {
		name = fmt.Sprintf("ID %d", targetID)
	}
	unameStr := ""
	if uname.Valid {
		unameStr = " @" + uname.String
	}

	rows, err := db.Query(`SELECT id, file_id, chat_id, created_at
		FROM events WHERE user_id=? AND event_type='photo'
		ORDER BY id DESC`, targetID)
	if err != nil {
		sendReply(ctx, b, msg, fmt.Sprintf("Ошибка: %v", err))
		return
	}
	defer rows.Close()

	type photoEntry struct {
		eventID   int
		fileID    string
		chatID    int64
		createdAt string
	}

	var photos []photoEntry
	for rows.Next() {
		var p photoEntry
		rows.Scan(&p.eventID, &p.fileID, &p.chatID, &p.createdAt)
		photos = append(photos, p)
	}

	if len(photos) == 0 {
		sendReply(ctx, b, msg, fmt.Sprintf("Нет фото от %s (ID: %d)", name, targetID))
		return
	}

	text := fmt.Sprintf("📸 Фото от %s%s (ID: %d) — %d шт.\n\n", name, unameStr, targetID, len(photos))

	keyboard := [][]models.InlineKeyboardButton{}
	limit := len(photos)
	if limit > 15 {
		limit = 15
	}

	for i := 0; i < limit; i++ {
		p := photos[i]
		note := ""
		if p.fileID == "" {
			note = " (нет file_id)"
		}
		keyboard = append(keyboard, []models.InlineKeyboardButton{
			{Text: fmt.Sprintf("#%d %s%s", p.eventID, fmtTime(p.createdAt), note), CallbackData: fmt.Sprintf("viewphoto_%d", p.eventID)},
		})
	}

	if len(photos) > 15 {
		text += fmt.Sprintf("Показаны последние 15 из %d.\n\n", len(photos))
	}

	text += "Нажми кнопку чтобы посмотреть фото."

	b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      msg.Chat.ID,
		Text:        text,
		ReplyMarkup: models.InlineKeyboardMarkup{InlineKeyboard: keyboard},
		ReplyParameters: &models.ReplyParameters{MessageID: msg.ID},
	})
}

func handleBan(ctx context.Context, b *bot.Bot, msg *models.Message) {
	parts := strings.Fields(msg.Text)
	if len(parts) < 2 {
		sendReply(ctx, b, msg, "Использование: /ban user_id")
		return
	}

	targetID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		sendReply(ctx, b, msg, "Неверный ID.")
		return
	}

	var chatID int64
	err = db.QueryRow(`SELECT chat_id FROM events WHERE user_id=? AND chat_type != 'private' ORDER BY id DESC LIMIT 1`, targetID).Scan(&chatID)
	if err != nil {
		sendReply(ctx, b, msg, fmt.Sprintf("Пользователь %d не найден в группах.", targetID))
		return
	}

	_, err = b.BanChatMember(ctx, &bot.BanChatMemberParams{ChatID: chatID, UserID: targetID})
	if err != nil {
		sendReply(ctx, b, msg, fmt.Sprintf("Ошибка: %v", err))
		return
	}

	sendReply(ctx, b, msg, fmt.Sprintf("✅ %d забанен в %d", targetID, chatID))
}

func handleCleanup(ctx context.Context, b *bot.Bot, msg *models.Message) {
	parts := strings.Fields(msg.Text)
	if len(parts) < 2 {
		sendReply(ctx, b, msg, "Использование: /cleanup 30 (дней)")
		return
	}

	days, err := strconv.Atoi(parts[1])
	if err != nil || days < 1 {
		sendReply(ctx, b, msg, "Неверное число.")
		return
	}

	result, err := db.Exec(`DELETE FROM events WHERE created_at < datetime('now', '-' || ? || ' days')`, days)
	if err != nil {
		sendReply(ctx, b, msg, "Ошибка.")
		return
	}

	affected, _ := result.RowsAffected()
	sendReply(ctx, b, msg, fmt.Sprintf("Удалено %d записей старше %d дней.", affected, days))
}
