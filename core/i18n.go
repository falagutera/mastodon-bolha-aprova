package core

import (
	"net/http"
	"slices"
	"strings"
)

const langCookie = "__Host-aprova-lang"

var supportedLangs = []string{"en", "pt-BR"}

var translations = map[string]map[string]string{
	"en": {
		"lang": "en", "title": "Aprova", "date_format": "Jan 2, 2006 15:04",
		"lede":  "Sign-up approvals for the moderation team. Sign in with your account on the instance.",
		"login": "Sign in", "logout": "Sign out", "dm_is_on": "You get direct messages from the bot about pending sign-ups.", "dm_is_off": "Direct messages from the bot are off for you.", "dm_turn_off": "Turn off", "dm_turn_on": "Turn on", "flash_prefs": "Preference saved.", "nav_token": "API token", "token_title": "API token", "token_help": "Use it as Authorization: Bearer in the API. It is shown only now.", "token_note": "It acts as you, with your role, and expires in 8 hours. Sign-out on the instance or losing the role ends it too. Don't share it.", "nav_queue": "Queue", "nav_audit": "Log", "modules": "Modules",
		"privacy":     "Only team members with a moderation role get in. Moderators see the applicant's username, e-mail and reason. aprova keeps the discussion, the reviews and a log of decisions; it doesn't keep IP addresses.",
		"queue_empty": "Nothing waiting.", "all": "All", "applicant": "Applicant", "email": "E-mail", "reason": "Reason", "reason_not_asked": "This app's sign-up form doesn't ask for one.", "created": "Requested",
		"status_new": "New", "status_reviewing": "In review", "status_ready": "Ready", "status_blocked": "Objected", "status_awaiting": "Waiting for answer", "status_escalated": "Escalated",
		"filter_new": "New", "filter_reviewing": "In review", "filter_awaiting": "Waiting for answer", "filter_escalated": "Escalated",
		"stale": "no answer for a while", "open": "Open",
		"check_bot_signal": "Looks automated", "check_disposable_email": "Disposable e-mail", "check_reason_given": "No reason given", "check_email_verified": "E-mail not confirmed",
		"check_ok_bot_signal": "Doesn't look automated", "check_ok_disposable_email": "E-mail isn't disposable", "check_ok_reason_given": "Reason given",
		"form_needs_text": "Required for this choice",
		"alerts":          "Alerts", "no_alerts": "No alerts", "checked": "Checked", "checked_fine": "No problem with",
		"checked_bot_signal": "signs of a bot", "checked_disposable_email": "disposable e-mail", "checked_reason_given": "blank reason", "checked_email_verified": "unconfirmed e-mail",
		"detail_random_reason": "the reason is a random word", "detail_dotted_gmail": "Gmail with many dots in the name", "detail_link_in_reason": "a link in the reason",
		"checks": "Checks", "reviews": "Reviews", "no_reviews": "No reviews yet.", "discussion": "Discussion", "no_comments": "No comments yet.",
		"verdict_approve": "would approve", "verdict_reject": "would reject", "verdict_info": "needs more information",
		"review": "Review", "review_approve": "I would approve", "review_approve_quorum": "Review and approve", "review_reject": "I would reject", "review_info": "Ask the applicant",
		"review_note":   "Note for the team (optional; required to reject)",
		"review_reason": "Justification", "review_reason_help": "Required to reject. The team sees it, the applicant doesn't.", "review_send": "Send review",
		"ask_title": "Ask the applicant", "ask_body": "Message (sent by e-mail)", "ask_send": "Send question",
		"ask_help": "The request waits for the answer. Their reply goes to the reply-to mailbox; record it here with \"Record answer\".",
		"edit":     "Edit", "edited": "edited", "save": "Save", "flash_edited": "Comment updated.", "question": "Question to the applicant (sent by e-mail)",
		"approve_now": "Approve now", "approve_note": "Internal note (only the team sees it)", "approve_note_required": "Someone on the team voted to reject, or a check failed: say why you approve anyway.", "approve_note_optional": "Optional. It stays in the request history; the applicant never sees it.",
		"reject_now": "Reject", "reject_reason": "Internal reason (only the team sees it)", "reject_mail": "The applicant gets the standard message, never the reason.",
		"reject_app_mails": "This app e-mails the applicant itself when rejected; aprova won't send another.",
		"escalate":         "Escalate to the senior team", "escalate_why": "Why (required)", "deescalate": "Take back to the team",
		"answer": "Record the applicant's answer", "answer_help": "Paste what the applicant replied. With reset on, earlier reviews stop counting.",
		"comment": "Comment", "send": "Send",
		"round": "Round", "round_current": "current", "round_closed": "closed, its reviews no longer count", "round_started": "Started with the applicant's answer above: reviews from earlier rounds no longer count. Review again.", "round_empty": "No reviews in this round yet.", "escalated_note": "Escalated: only the senior team decides, everyone can still comment.",
		"awaiting_note": "Asked the applicant on", "decided": "Decided", "decision_approved": "Approved", "decision_rejected": "Rejected", "decision_gone": "No longer pending",
		"counts": "counts", "quorum": "Quorum", "welcome_note": "The app sends its own welcome e-mail.",
		"kind_comment": "commented", "kind_review_approve": "would approve", "kind_review_reject": "would reject", "kind_escalate": "escalated",
		"kind_deescalate": "took it back to the team", "kind_info_question": "asked the applicant", "kind_info_answer": "recorded the answer",
		"kind_reject_reason": "rejected, reason", "kind_approve_note": "approved, note", "kind_system": "aprova", "system_conflict": "Reviews disagree: escalated.",
		"bulk_reject": "Reject selected", "bulk_reason": "Internal reason for all selected", "select": "Select",
		"audit_title": "Decision log", "audit_empty": "Nothing yet.", "who": "Who", "what": "What", "where": "Where", "result": "Result",
		"flash_approved": "Approved.", "flash_rejected": "Rejected, and the applicant was told.", "flash_reviewed": "Review saved.", "flash_escalated": "Escalated.",
		"flash_deescalated": "Back with the team.", "flash_answered": "Answer recorded.", "flash_commented": "Comment added.", "flash_asked": "Question sent to the applicant.",
		"flash_gone": "This request is no longer pending: someone decided it elsewhere.", "flash_already": "Already decided.", "flash_bulk": "Selected requests rejected.", "form_bulk_failed": "Some selected requests could not be rejected: the app refused. They are still in the queue; the aprova log has the details.",
		"flash_mail_failed": "Rejected, but the e-mail to the applicant failed. It is in the log.", "flash_no_mail": "Rejected. E-mail is not set up, so the applicant was not told.",
		"form_too_long": "That text is too long.", "form_empty": "Write something.", "form_reason_required": "Rejecting needs a reason.",
		"form_question_required": "Write the question for the applicant.", "form_bad_verdict": "Pick a review.", "form_mail": "The e-mail to the applicant couldn't be sent, so nothing changed.",
		"form_note_required": "There is a reject or a failed check: say why you approve.", "form_not_waiting": "No question to the applicant is waiting for an answer.", "form_why_required": "Say why you escalate.",
		"error_title": "Something went wrong", "error_expired": "This page expired. Try again.", "error_denied": "Access was not granted.",
		"error_instance": "The app didn't answer as expected.", "error_unavailable": "Temporarily unavailable.", "error_role": "Your account has no moderation role here.",
		"error_not_found": "Not found.", "error_unauthorized": "The app refused the credentials.", "error_forbidden": "Your role can't do that.", "home": "Back",
		"service_unavailable": "Couldn't read this service right now.", "service_unauthorized": "The service refused the credentials.",
		"source":                "Source code",
		"mail_reject_subject":   "Your request to join {{.Service}}",
		"mail_reject_body":      "Hello,\n\nThank you for your interest in {{.Service}}. At the moment we can't accept your request.\n\nThe {{.Service}} team\n",
		"mail_info_subject":     "About your request to join {{.Service}}",
		"mail_info_body":        "Hello,\n\nWe are reviewing your request to join {{.Service}} as {{.Username}}, and we need a little more information:\n\n{{.Question}}\n\nJust reply to this e-mail.\n\nThe {{.Service}} team\n",
		"mm_escalated":          "%[1]s escalated a sign-up on %[2]s (@%[3]s) to the senior team: %[4]s",
		"mm_escalated_conflict": "Reviews disagree on a sign-up on %[2]s (@%[3]s), so it went to the senior team: %[4]s",
		"mm_digest":             "%[1]d sign-ups waiting in %[2]s: %[3]s. %[4]s",
	},
	"pt-BR": {
		"lang": "pt-BR", "title": "Aprova", "date_format": "02/01/2006 15:04",
		"lede":  "Aprovação de cadastros para a equipe de moderação. Entre com sua conta da instância.",
		"login": "Entrar", "logout": "Sair", "dm_is_on": "Você recebe DMs do bot sobre cadastros pendentes.", "dm_is_off": "As DMs do bot estão desligadas para você.", "dm_turn_off": "Desligar", "dm_turn_on": "Ligar", "flash_prefs": "Preferência salva.", "nav_token": "Token da API", "token_title": "Token da API", "token_help": "Use como Authorization: Bearer na API. Ele só aparece agora.", "token_note": "Ele age como você, com o seu cargo, e vence em 8 horas. Sair na instância ou perder o cargo também o encerra. Não compartilhe.", "nav_queue": "Fila", "nav_audit": "Registro", "modules": "Módulos",
		"privacy":     "Só entra quem tem cargo de moderação. A equipe vê o usuário, o e-mail e o motivo de quem pediu a conta. O aprova guarda a discussão, as revisões e o registro das decisões; não guarda endereço IP.",
		"queue_empty": "Nada esperando.", "all": "Todos", "applicant": "Pedido", "email": "E-mail", "reason": "Motivo", "reason_not_asked": "O cadastro deste app não pede motivo.", "created": "Pedido em",
		"status_new": "Novo", "status_reviewing": "Em revisão", "status_ready": "Pronto", "status_blocked": "Com objeção", "status_awaiting": "Aguardando resposta", "status_escalated": "Escalado",
		"filter_new": "Novos", "filter_reviewing": "Em revisão", "filter_awaiting": "Aguardando resposta", "filter_escalated": "Escalados",
		"stale": "sem resposta há tempo", "open": "Abrir",
		"check_bot_signal": "Parece automatizado", "check_disposable_email": "E-mail descartável", "check_reason_given": "Sem motivo", "check_email_verified": "E-mail não confirmado",
		"check_ok_bot_signal": "Não parece automatizado", "check_ok_disposable_email": "E-mail não descartável", "check_ok_reason_given": "Com motivo",
		"form_needs_text": "Obrigatório para essa escolha",
		"alerts":          "Alertas", "no_alerts": "Nenhum alerta", "checked": "Conferido", "checked_fine": "Sem problema",
		"checked_bot_signal": "padrão de robô", "checked_disposable_email": "e-mail descartável", "checked_reason_given": "motivo em branco", "checked_email_verified": "e-mail não confirmado",
		"detail_random_reason": "o motivo é uma palavra aleatória", "detail_dotted_gmail": "Gmail com muitos pontos no nome", "detail_link_in_reason": "link no motivo",
		"checks": "Verificações", "reviews": "Revisões", "no_reviews": "Nenhuma revisão ainda.", "discussion": "Discussão", "no_comments": "Nenhum comentário ainda.",
		"verdict_approve": "aprovaria", "verdict_reject": "recusaria", "verdict_info": "precisa de mais informações",
		"review": "Revisar", "review_approve": "Eu aprovaria", "review_approve_quorum": "Revisar e aprovar", "review_reject": "Eu recusaria", "review_info": "Perguntar a quem pediu",
		"review_note":   "Nota para a equipe (opcional; obrigatória para recusar)",
		"review_reason": "Justificativa", "review_reason_help": "Obrigatória para recusar. A equipe vê, quem pediu não.", "review_send": "Enviar revisão",
		"ask_title": "Perguntar a quem pediu", "ask_body": "Mensagem (vai por e-mail)", "ask_send": "Enviar pergunta",
		"ask_help": "O pedido fica aguardando a resposta. A resposta chega na caixa de resposta; registre aqui em \"Registrar resposta\".",
		"edit":     "Editar", "edited": "editado", "save": "Salvar", "flash_edited": "Comentário atualizado.", "question": "Pergunta para quem pediu (vai por e-mail)",
		"approve_now": "Aprovar agora", "approve_note": "Nota interna (só a equipe vê)", "approve_note_required": "Alguém da equipe votou por recusar, ou uma verificação falhou: diga por que aprova mesmo assim.", "approve_note_optional": "Opcional. Fica no histórico do pedido; quem pediu nunca vê.",
		"reject_now": "Recusar", "reject_reason": "Motivo interno (só a equipe vê)", "reject_mail": "Quem pediu recebe a mensagem padrão, nunca o motivo.",
		"reject_app_mails": "Este app já avisa quem pediu quando recusa; o aprova não manda outro e-mail.",
		"escalate":         "Escalar para os Sábios", "escalate_why": "Por quê (obrigatório)", "deescalate": "Devolver para a equipe",
		"answer": "Registrar a resposta de quem pediu", "answer_help": "Cole o que a pessoa respondeu. Com o reinício ligado, as revisões anteriores deixam de contar.",
		"comment": "Comentar", "send": "Enviar",
		"round": "Rodada", "round_current": "atual", "round_closed": "encerrada, os votos daqui não contam mais", "round_started": "Começou com a resposta de quem pediu: os votos das rodadas anteriores não contam mais. Revise de novo.", "round_empty": "Ainda sem revisões nesta rodada.", "escalated_note": "Escalado: só os Sábios decidem; todos ainda podem comentar.",
		"awaiting_note": "Pergunta enviada em", "decided": "Decidido", "decision_approved": "Aprovado", "decision_rejected": "Recusado", "decision_gone": "Não está mais pendente",
		"counts": "conta", "quorum": "Quórum", "welcome_note": "O app manda o próprio e-mail de boas-vindas.",
		"kind_comment": "comentou", "kind_review_approve": "aprovaria", "kind_review_reject": "recusaria", "kind_escalate": "escalou",
		"kind_deescalate": "devolveu para a equipe", "kind_info_question": "perguntou a quem pediu", "kind_info_answer": "registrou a resposta",
		"kind_reject_reason": "recusou, motivo", "kind_approve_note": "aprovou, nota", "kind_system": "aprova", "system_conflict": "Revisões divergentes: escalado.",
		"bulk_reject": "Recusar selecionados", "bulk_reason": "Motivo interno para todos os selecionados", "select": "Selecionar",
		"audit_title": "Registro de decisões", "audit_empty": "Nada ainda.", "who": "Quem", "what": "O quê", "where": "Onde", "result": "Resultado",
		"flash_approved": "Aprovado.", "flash_rejected": "Recusado, e quem pediu foi avisado.", "flash_reviewed": "Revisão salva.", "flash_escalated": "Escalado.",
		"flash_deescalated": "De volta com a equipe.", "flash_answered": "Resposta registrada.", "flash_commented": "Comentário adicionado.", "flash_asked": "Pergunta enviada para quem pediu.",
		"flash_gone": "Este pedido não está mais pendente: alguém decidiu por outro lugar.", "flash_already": "Já foi decidido.", "flash_bulk": "Pedidos selecionados recusados.", "form_bulk_failed": "Alguns pedidos selecionados não foram recusados: o app recusou. Eles continuam na fila; o log do aprova tem os detalhes.",
		"flash_mail_failed": "Recusado, mas o e-mail para quem pediu falhou. Está no registro.", "flash_no_mail": "Recusado. O e-mail não está configurado, então quem pediu não foi avisado.",
		"form_too_long": "Esse texto é longo demais.", "form_empty": "Escreva algo.", "form_reason_required": "Recusar exige um motivo.",
		"form_question_required": "Escreva a pergunta para quem pediu.", "form_bad_verdict": "Escolha uma revisão.", "form_mail": "Não deu para mandar o e-mail para quem pediu, então nada mudou.",
		"form_note_required": "Há uma recusa ou verificação falhando: diga por que aprova.", "form_not_waiting": "Não há pergunta a quem pediu esperando resposta.", "form_why_required": "Diga por que está escalando.",
		"error_title": "Algo deu errado", "error_expired": "Esta página expirou. Tente de novo.", "error_denied": "O acesso não foi autorizado.",
		"error_instance": "O app não respondeu como esperado.", "error_unavailable": "Indisponível no momento.", "error_role": "Sua conta não tem cargo de moderação aqui.",
		"error_not_found": "Não encontrado.", "error_unauthorized": "O app recusou as credenciais.", "error_forbidden": "Seu cargo não permite isso.", "home": "Voltar",
		"service_unavailable": "Não deu para ler este serviço agora.", "service_unauthorized": "O serviço recusou as credenciais.",
		"source":                "Código-fonte",
		"mail_reject_subject":   "Seu pedido de conta em {{.Service}}",
		"mail_reject_body":      "Olá,\n\nObrigado pelo interesse em {{.Service}}. No momento não poderemos atender seu pedido.\n\nEquipe {{.Service}}\n",
		"mail_info_subject":     "Sobre seu pedido de conta em {{.Service}}",
		"mail_info_body":        "Olá,\n\nEstamos analisando seu pedido de conta em {{.Service}} como {{.Username}} e precisamos de um pouco mais de informação:\n\n{{.Question}}\n\nÉ só responder este e-mail.\n\nEquipe {{.Service}}\n",
		"mm_escalated":          "%[1]s escalou um cadastro em %[2]s (@%[3]s) para os Sábios: %[4]s",
		"mm_escalated_conflict": "Revisões divergentes num cadastro em %[2]s (@%[3]s), então ele foi para os Sábios: %[4]s",
		"mm_digest":             "%[1]d cadastros esperando em %[2]s: %[3]s. %[4]s",
	},
}

func strs(lang string) map[string]string {
	if t, ok := translations[lang]; ok {
		return t
	}
	return translations["en"]
}

func language(r *http.Request, fallback string) string {
	if c, err := r.Cookie(langCookie); err == nil && slices.Contains(supportedLangs, c.Value) {
		return c.Value
	}
	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		tag := strings.ToLower(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]))
		switch {
		case strings.HasPrefix(tag, "pt"):
			return "pt-BR"
		case strings.HasPrefix(tag, "en"):
			return "en"
		}
	}
	return fallback
}

func setLanguage(w http.ResponseWriter, lang string) {
	if lang == "pt" {
		lang = "pt-BR"
	}
	if !slices.Contains(supportedLangs, lang) {
		return
	}
	http.SetCookie(w, &http.Cookie{Name: langCookie, Value: lang, Path: "/", MaxAge: 365 * 24 * 3600, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
}
