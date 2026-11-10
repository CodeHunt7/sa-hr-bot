package handlers

import (
	"context"
	"errors"
	"net/url"

	tele "gopkg.in/telebot.v3"

	"sa-hr-bot/internal/db"
)

const (
	subscriptionChannel channelUsername = "@sa_jelatinka"
	subscriptionURL                     = "https://t.me/sa_jelatinka"

	welcomeMessage = `Привет. Меня зовут Катя Желатинка, я собрала для тебя тренажёр для отработки технических собеседований системного аналитика.

Это не чат-бот общего назначения. Под капотом этого ИИ-агента собраны 58 вопросов, которые задают на собесах в 2026 году.

Формат простой: сначала 4 коротких вопроса про тебя, чтобы лучше понять твой запрос. Дальше цикл вопросов с разбором каждого ответа. Фидбек получаешь сразу, без ожидания.

<b>Для старта подпишись на мой канал 👇🏻</b>`

	subscriptionRequiredMessage = "Пока не вижу тебя в канале 🙈 <b>Подпишись и нажми «Я в деле» еще раз.</b>"
	subscriptionErrorMessage    = "Не получилось проверить подписку. Попробуй нажать «Я в деле» ещё раз чуть позже."

	instructionMessage = `Представь, что готовишься к экзамену на права для авто, все тоже самое, только готовимся к собеседованию.

Я буду задавать тебе основной вопрос и два дополнительных, а после буду давать обратную связь и разъяснение по твоим ответам.

Прежде чем начнём - 4 вопроса, чтобы подобрать нужный уровень сложности. Начинаем?`
)

var btnCheckSubscription = tele.Btn{Unique: "check_subscription"}

type channelUsername string

func (u channelUsername) Recipient() string {
	return string(u)
}

func subscriptionMenu(checkLabel string) *tele.ReplyMarkup {
	menu := &tele.ReplyMarkup{}
	subscribe := menu.URL("Подписаться на канал", subscriptionURL)
	check := menu.Data(checkLabel, btnCheckSubscription.Unique)
	menu.Inline(menu.Row(subscribe), menu.Row(check))
	return menu
}

func (h *Handler) handleStart(c tele.Context) error {
	telegramID := senderID(c)
	unlock := h.lockStudent(telegramID)
	defer unlock()
	ctx, cancel := newHandlerContext()
	defer cancel()

	_, err := h.repo.GetStudentByTelegramID(ctx, telegramID)
	if errors.Is(err, db.ErrStudentNotFound) {
		photo := &tele.Photo{File: tele.FromDisk(welcomePhotoPath), Caption: welcomeMessage}
		return c.Send(photo, tele.ModeHTML, subscriptionMenu("Я готов"))
	}
	if err != nil {
		h.logger.Error("get student", "error", err)
		return c.Send(genericErrorMessage)
	}

	session, err := h.repo.GetActiveSession(ctx, telegramID)
	switch {
	case errors.Is(err, db.ErrNoActiveSession):
		return h.startFreshSession(ctx, c, telegramID)
	case err != nil:
		h.logger.Error("get active session", "error", err)
		return c.Send(genericErrorMessage)
	case session.Status == db.SessionStatusInstruction:
		return h.sendInstruction(ctx, c, session)
	default:
		return c.Send("У вас уже есть активная сессия. Начать заново или продолжить?", restartMenu)
	}
}

func (h *Handler) handleSubscription(c tele.Context, chatMemberOf func(tele.Recipient, tele.Recipient) (*tele.ChatMember, error)) error {
	if err := c.Respond(); err != nil {
		h.logger.Warn("could not acknowledge subscription callback", "telegram_id", senderID(c))
	}

	telegramID := senderID(c)
	if telegramID == 0 {
		return nil
	}
	unlock := h.lockStudent(telegramID)
	defer unlock()
	ctx, cancel := newHandlerContext()
	defer cancel()

	_, err := h.repo.GetStudentByTelegramID(ctx, telegramID)
	if err == nil {
		// An old button must not replace an active session.
		session, err := h.repo.GetActiveSession(ctx, telegramID)
		switch {
		case errors.Is(err, db.ErrNoActiveSession):
			return h.startFreshSession(ctx, c, telegramID)
		case err != nil:
			h.logger.Error("get session after subscription check", "error", err)
			return c.Send(genericErrorMessage)
		case session.Status == db.SessionStatusInstruction:
			return h.sendInstruction(ctx, c, session)
		default:
			return nil
		}
	}
	if !errors.Is(err, db.ErrStudentNotFound) {
		h.logger.Error("get student for subscription check", "error", err)
		return c.Send(genericErrorMessage)
	}

	member, err := chatMemberOf(subscriptionChannel, c.Sender())
	if err != nil || member == nil {
		// The URL in a transport error includes the Telegram bot token.
		var transportErr *url.Error
		if errors.As(err, &transportErr) {
			err = transportErr.Err
		}
		h.logger.Warn("check Telegram channel subscription", "error", err)
		return c.Send(subscriptionErrorMessage, subscriptionMenu("Я в деле"))
	}
	if !isSubscribed(member) {
		return c.Send(subscriptionRequiredMessage, tele.ModeHTML, subscriptionMenu("Я в деле"))
	}

	if err := h.repo.RegisterStudent(ctx, telegramID, displayName(c.Sender())); err != nil {
		h.logger.Error("register student", "error", err)
		return c.Send(genericErrorMessage)
	}
	return h.startFreshSession(ctx, c, telegramID)
}

func isSubscribed(member *tele.ChatMember) bool {
	switch member.Role {
	case tele.Creator, tele.Administrator, tele.Member:
		return true
	case tele.Restricted:
		return member.Member
	default:
		return false
	}
}

func (h *Handler) startFreshSession(ctx context.Context, c tele.Context, telegramID int64) error {
	session, err := h.repo.StartSession(ctx, telegramID)
	if err != nil {
		h.logger.Error("start session", "error", err)
		return c.Send(genericErrorMessage)
	}
	return h.sendInstruction(ctx, c, session)
}

func (h *Handler) sendInstruction(ctx context.Context, c tele.Context, session *db.Session) error {
	if err := c.Send(instructionMessage); err != nil {
		return err
	}
	if err := h.repo.AdvancePhase(ctx, session.ID, db.SessionStatusQualification); err != nil {
		h.logger.Error("advance phase", "error", err, "session_id", session.ID)
		return c.Send(genericErrorMessage)
	}
	return c.Send(currentGradeQuestion, currentGradeMenu)
}
