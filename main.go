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
	cleanup   func()
	db        *sql.DB
)

func main() {
	godotenv.Load()

	token := os.Getenv("token")

	for _, idStr := range strings.Split(os.Getenv("admin_ids"), ",") {
		idStr = strings.TrimSpace(idStr)
		if id, err := strconv.ParseInt(idStr, 10, 64); err == nil && id > 0 {
			adminIDs[id] = true
		}
	}

	initDB()

	httpCli, cleanup = startXrayAndProxy()
	defer cleanup()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	opts := []bot.Option{
		bot.WithDefaultHandler(handler),
		bot.WithHTTPClient(30*time.Second, httpCli),
		bot.WithAllowedUpdates([]string{"message", "callback_query", "my_chat_member"}),
	}

	b, err := bot.New(token, opts...)
	if err != nil {
		panic(err)
	}

	b.SetMyCommands(ctx, &bot.SetMyCommandsParams{
		Commands: []models.BotCommand{
			{Command: "start", Description: "Запуск бота"},
			{Command: "help", Description: "Помощь"},
			{Command: "bw", Description: "Обычный ЧБ"},
			{Command: "bwl", Description: "Светлый ЧБ"},
			{Command: "bwd", Description: "Тёмный ЧБ"},
			{Command: "mode", Description: "Текущий режим"},
		},
		Scope: &models.BotCommandScopeDefault{},
	})

	b.Start(ctx)
}

func isAdmin(id int64) bool {
	return adminIDs[id]
}

func getDataDir() string {
	dir := os.Getenv("DATA_DIR")
	if dir == "" {
		dir = getWorkDir()
	}
	os.MkdirAll(dir, 0755)
	return dir
}

func initDB() {
	var err error
	db, err = sql.Open("sqlite3", getDataDir()+"/bot_data.db?_journal_mode=WAL")
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

// ──────────────────── INLINE KEYBOARD ────────────────────

func modeKeyboard(userID int64) models.InlineKeyboardMarkup {
	mode := getMode(userID)
	current := fmt.Sprintf("Текущий: %s %s", modeEmoji(mode), modeName(mode))

	return models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{
				{Text: "🔘 Обычный", CallbackData: "setmode_0"},
				{Text: "☀️ Светлый", CallbackData: "setmode_1"},
				{Text: "🌑 Тёмный", CallbackData: "setmode_2"},
			},
			{
				{Text: current, CallbackData: "noop"},
			},
		},
	}
}

func adminKeyboard() models.InlineKeyboardMarkup {
	return models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{
				{Text: "📊 Статистика", CallbackData: "adm_stats"},
				{Text: "📸 Фото", CallbackData: "adm_photos"},
				{Text: "👥 Юзеры", CallbackData: "adm_users"},
			},
		},
	}
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
	if update.MyChatMember != nil {
		t := update.MyChatMember.NewChatMember.Type
		if t == "member" || t == "administrator" || t == "creator" {
			b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: update.MyChatMember.Chat.ID,
				Text: "Бот добавлен! Отправьте фото — получите ЧБ версию.\n\n" +
					"/bw /bwl /bwd — смена режима\n" +
					"Ответ на фото /bw — обработка конкретного фото",
			})
		}
	}
}

func handleMessage(ctx context.Context, b *bot.Bot, msg *models.Message) {
	user := msg.From
	text := msg.Text

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

	// ─── Group commands ───
	if msg.Chat.Type != "private" {
		switch text {
		case "/bw", "/bwl", "/bwd":
			var mode bwMode
			switch text {
			case "/bw":
				mode = bwNormal
			case "/bwl":
				mode = bwLight
			case "/bwd":
				mode = bwDark
			}
			setMode(user.ID, mode)

			if msg.ReplyToMessage != nil && len(msg.ReplyToMessage.Photo) > 0 {
				processPhotoFrom(ctx, b, msg, msg.ReplyToMessage, mode)
				return
			}

			b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: msg.Chat.ID,
				Text:   fmt.Sprintf("%s: %s %s", user.FirstName, modeEmoji(mode), modeName(mode)),
				ReplyParameters: &models.ReplyParameters{
					MessageID: msg.ID,
				},
			})
			return
		case "/mode":
			mode := getMode(user.ID)
			b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID:      msg.Chat.ID,
				Text:        fmt.Sprintf("Режим: %s %s", modeEmoji(mode), modeName(mode)),
				ReplyMarkup: modeKeyboard(user.ID),
				ReplyParameters: &models.ReplyParameters{
					MessageID: msg.ID,
				},
			})
			return
		}

		if msg.Photo != nil {
			processPhoto(ctx, b, msg)
			return
		}
		return
	}

	// ─── Private chat ───

	// Reply to photo with /bw /bwl /bwd
	if msg.ReplyToMessage != nil && len(msg.ReplyToMessage.Photo) > 0 {
		switch text {
		case "/bw", "/bwl", "/bwd":
			var mode bwMode
			switch text {
			case "/bw":
				mode = bwNormal
			case "/bwl":
				mode = bwLight
			case "/bwd":
				mode = bwDark
			}
			setMode(user.ID, mode)
			processPhotoFrom(ctx, b, msg, msg.ReplyToMessage, mode)
			return
		}
	}

	switch text {
	case "/start", "/help":
		mode := getMode(user.ID)
		photoCount := queryInt("SELECT COUNT(*) FROM events WHERE user_id=? AND event_type='photo'", user.ID)
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: msg.Chat.ID,
			Text: fmt.Sprintf("Режим: %s %s\nФото обработано: %d\n\n"+
				"Отправьте фото — получите ЧБ версию.\n"+
				"Ответ на фото /bw — обработка конкретного фото.\n\n"+
				"/bw /bwl /bwd — смена режима\n"+
				"/mode — текущий режим с кнопками",
				modeEmoji(mode), modeName(mode), photoCount),
			ReplyMarkup: modeKeyboard(user.ID),
		})
		return
	case "/bw", "/bwl", "/bwd":
		var mode bwMode
		switch text {
		case "/bw":
			mode = bwNormal
		case "/bwl":
			mode = bwLight
		case "/bwd":
			mode = bwDark
		}
		setMode(user.ID, mode)
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:      msg.Chat.ID,
			Text:        fmt.Sprintf("Режим: %s %s", modeEmoji(mode), modeName(mode)),
			ReplyMarkup: modeKeyboard(user.ID),
		})
		return
	case "/mode":
		mode := getMode(user.ID)
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:      msg.Chat.ID,
			Text:        fmt.Sprintf("Режим: %s %s", modeEmoji(mode), modeName(mode)),
			ReplyMarkup: modeKeyboard(user.ID),
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

// ──────────────────── CALLBACKS ────────────────────

func handleCallback(ctx context.Context, b *bot.Bot, cb *models.CallbackQuery) {
	data := cb.Data

	// Mode switch: setmode_0 / setmode_1 / setmode_2
	if strings.HasPrefix(data, "setmode_") {
		modeIdx, _ := strconv.Atoi(strings.TrimPrefix(data, "setmode_"))
		mode := bwMode(modeIdx)
		setMode(cb.From.ID, mode)

		inner := cb.Message.Message
		if inner != nil {
			b.EditMessageText(ctx, &bot.EditMessageTextParams{
				ChatID:      inner.Chat.ID,
				MessageID:   inner.ID,
				Text:        fmt.Sprintf("Режим: %s %s", modeEmoji(mode), modeName(mode)),
				ReplyMarkup: modeKeyboard(cb.From.ID),
			})
		}

		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
			CallbackQueryID: cb.ID,
			Text:            fmt.Sprintf("Режим: %s %s", modeEmoji(mode), modeName(mode)),
		})
		return
	}

	// View photo by event ID
	if strings.HasPrefix(data, "viewphoto_") {
		eid, _ := strconv.Atoi(strings.TrimPrefix(data, "viewphoto_"))
		sendPhotoByEventID(ctx, b, cb, eid)
		return
	}

	// Admin callbacks
	if !isAdmin(cb.From.ID) {
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cb.ID, Text: "Нет доступа", ShowAlert: true})
		return
	}

	switch data {
	case "adm_stats":
		adminStatsCallback(ctx, b, cb)
	case "adm_photos":
		adminPhotosCallback(ctx, b, cb)
	case "adm_users":
		adminUsersCallback(ctx, b, cb)
	case "noop":
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cb.ID})
	}
}

func sendPhotoByEventID(ctx context.Context, b *bot.Bot, cb *models.CallbackQuery, eventID int) {
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

	if cb.From.ID != uid && !isAdmin(cb.From.ID) {
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cb.ID, Text: "Нет доступа", ShowAlert: true})
		return
	}

	name := fmtName(firstName, uname, uid)
	caption := fmt.Sprintf("#%d | %s | %s | chat %d", eventID, fmtTime(createdAt), name, chatID)

	b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cb.ID})

	b.SendPhoto(ctx, &bot.SendPhotoParams{
		ChatID:  cb.Message.Message.Chat.ID,
		Photo:   &models.InputFileString{Data: fileID},
		Caption: caption,
	})
}

func adminStatsCallback(ctx context.Context, b *bot.Bot, cb *models.CallbackQuery) {
	totalEvents := queryInt("SELECT COUNT(*) FROM events")
	totalPhotos := queryInt("SELECT COUNT(*) FROM events WHERE event_type='photo'")
	totalUsers := queryInt("SELECT COUNT(DISTINCT user_id) FROM events")

	text := fmt.Sprintf("📊 Событий: %d | Фото: %d | Юзеров: %d", totalEvents, totalPhotos, totalUsers)

	b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cb.ID, Text: text, ShowAlert: true})
}

func adminPhotosCallback(ctx context.Context, b *bot.Bot, cb *models.CallbackQuery) {
	inner := cb.Message.Message
	if inner == nil {
		return
	}

	rows, err := db.Query(`SELECT id, user_id, username, first_name, chat_id, created_at
		FROM events WHERE event_type='photo'
		ORDER BY id DESC LIMIT 10`)
	if err != nil {
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cb.ID, Text: "Ошибка", ShowAlert: true})
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

		name := fmtName(firstName, username, uid)
		text += fmt.Sprintf("#%d | %s | %s | chat %d\n", eventID, fmtTime(createdAt), name, chatID)

		keyboard = append(keyboard, []models.InlineKeyboardButton{
			{Text: fmt.Sprintf("🔍 #%d", eventID), CallbackData: fmt.Sprintf("viewphoto_%d", eventID)},
		})
	}

	text += "\nНажми кнопку чтобы посмотреть."

	b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cb.ID})

	b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      inner.Chat.ID,
		Text:        text,
		ReplyMarkup: models.InlineKeyboardMarkup{InlineKeyboard: keyboard},
	})
}

func adminUsersCallback(ctx context.Context, b *bot.Bot, cb *models.CallbackQuery) {
	inner := cb.Message.Message
	if inner == nil {
		return
	}

	rows, err := db.Query(`SELECT user_id, username, first_name, COUNT(*) as cnt, MAX(created_at) as last_seen
		FROM events GROUP BY user_id ORDER BY last_seen DESC`)
	if err != nil {
		b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cb.ID, Text: "Ошибка", ShowAlert: true})
		return
	}
	defer rows.Close()

	text := "👥 Пользователи:\n\n"

	for rows.Next() {
		var uid int64
		var username, firstName sql.NullString
		var cnt int
		var lastSeen string

		rows.Scan(&uid, &username, &firstName, &cnt, &lastSeen)

		name := fmtName(firstName, username, uid)
		text += fmt.Sprintf("%d | %s | %d | %s\n", uid, name, cnt, fmtTime(lastSeen))
	}

	b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cb.ID})

	b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: inner.Chat.ID,
		Text:   text,
	})
}

// ──────────────────── PHOTO PROCESSING ────────────────────

func processPhoto(ctx context.Context, b *bot.Bot, msg *models.Message) {
	photo := msg.Photo[len(msg.Photo)-1]
	mode := getMode(msg.From.ID)

	file, err := b.GetFile(ctx, &bot.GetFileParams{FileID: photo.FileID})
	if err != nil {
		return
	}

	imgData, err := downloadFile(file.FilePath)
	if err != nil {
		return
	}

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

func processPhotoFrom(ctx context.Context, b *bot.Bot, cmdMsg *models.Message, photoMsg *models.Message, mode bwMode) {
	photo := photoMsg.Photo[len(photoMsg.Photo)-1]

	file, err := b.GetFile(ctx, &bot.GetFileParams{FileID: photo.FileID})
	if err != nil {
		sendReply(ctx, b, cmdMsg, "Ошибка скачивания.")
		return
	}

	imgData, err := downloadFile(file.FilePath)
	if err != nil {
		sendReply(ctx, b, cmdMsg, "Ошибка загрузки.")
		return
	}

	logEvent(cmdMsg.From.ID, cmdMsg.From.Username, cmdMsg.From.FirstName, cmdMsg.From.LastName, string(cmdMsg.Chat.Type), cmdMsg.Chat.ID, cmdMsg.ID, "photo_reply", photo.FileID)

	img, imgType, err := decodeImage(imgData)
	if err != nil {
		sendReply(ctx, b, cmdMsg, "Не удалось декодировать.")
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
		ChatID: cmdMsg.Chat.ID,
		Photo:  &models.InputFileUpload{Data: bytes.NewReader(buf.Bytes())},
		ReplyParameters: &models.ReplyParameters{
			MessageID: cmdMsg.ID,
		},
	})
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

	b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: msg.Chat.ID,
		Text:   fmt.Sprintf("📊 Событий: %d | Фото: %d | Юзеров: %d | Сегодня: %d", totalEvents, totalPhotos, totalUsers, todayPhotos),
		ReplyParameters: &models.ReplyParameters{MessageID: msg.ID},
	})
}

func showAdminUsers(ctx context.Context, b *bot.Bot, msg *models.Message) {
	rows, err := db.Query(`SELECT user_id, username, first_name, COUNT(*) as cnt, MAX(created_at) as last_seen
		FROM events GROUP BY user_id ORDER BY last_seen DESC`)
	if err != nil {
		return
	}
	defer rows.Close()

	text := "👥 Пользователи:\n\n"

	for rows.Next() {
		var uid int64
		var username, firstName sql.NullString
		var cnt int
		var lastSeen string

		rows.Scan(&uid, &username, &firstName, &cnt, &lastSeen)

		name := fmtName(firstName, username, uid)
		text += fmt.Sprintf("%d | %s | %d | %s\n", uid, name, cnt, fmtTime(lastSeen))
	}

	b.SendMessage(ctx, &bot.SendMessageParams{ChatID: msg.Chat.ID, Text: text, ReplyParameters: &models.ReplyParameters{MessageID: msg.ID}})
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

		name := fmtName(firstName, username, uid)
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

	for rows.Next() {
		var eventID int
		var uid int64
		var username, firstName sql.NullString
		var chatID int64
		var createdAt string

		rows.Scan(&eventID, &uid, &username, &firstName, &chatID, &createdAt)

		name := fmtName(firstName, username, uid)
		text += fmt.Sprintf("#%d | %s | %s | chat %d\n", eventID, fmtTime(createdAt), name, chatID)
	}

	text += "\n/view ID — фото | /ban uid — бан"

	b.SendMessage(ctx, &bot.SendMessageParams{ChatID: msg.Chat.ID, Text: text, ReplyParameters: &models.ReplyParameters{MessageID: msg.ID}})
}
