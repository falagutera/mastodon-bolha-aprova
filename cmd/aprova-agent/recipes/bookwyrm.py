# aprova-agent recipe for BookWyrm invite requests (tested on 0.9.4).
# Run by aprova-agent inside the BookWyrm image, with BookWyrm's own environment:
#
#   python bookwyrm.py pending | get <id> | approve <id> | reject <id>
#
# Prints one JSON object on its last line. BookWyrm has no username at this
# point: the person picks one through the invite link. Approve does what the
# admin page does (a one-use invite, e-mailed by BookWyrm). Reject only sets
# "ignored" (the admin page toggles it, so this never un-ignores); BookWyrm
# sends nothing, aprova tells the applicant.

import json
import os
import re
import sys


def answer(v):
    print()
    print(json.dumps(v, ensure_ascii=False))
    sys.exit(0)


action = sys.argv[1] if len(sys.argv) > 1 else ""
rid = sys.argv[2] if len(sys.argv) > 2 else ""
if action not in ("pending", "get", "approve", "reject"):
    answer({"error": "unknown action"})
if action != "pending" and not re.fullmatch(r"[0-9]{1,19}", rid):
    answer({"request": None} if action == "get" else {"gone": True})

root = os.environ.get("APROVA_AGENT_BOOKWYRM_ROOT", "/app")
sys.path.insert(0, root)
os.chdir(root)
os.environ.setdefault("DJANGO_SETTINGS_MODULE", "bookwyrm.settings")

try:
    import django

    django.setup()
    from django.db import transaction

    from bookwyrm import emailing, models

    def is_open(r):
        return r.invite_id is None and not r.ignored

    def row(r):
        local = (r.email or "").split("@")[0]
        return {
            "id": str(r.id),
            "username": local,
            "email": r.email or "",
            "reason": r.answer or "",
            "created": r.created_date.isoformat() if r.created_date else None,
        }

    def inviter():
        # Invites belong to a user; the admin who would have clicked.
        name = os.environ.get("APROVA_AGENT_BOOKWYRM_INVITER", "")
        q = models.User.objects.filter(local=True, is_active=True)
        if name:
            return q.get(localname=name)
        return q.filter(is_superuser=True).order_by("id").first()

    if action == "pending":
        reqs = models.InviteRequest.objects.filter(invite__isnull=True, ignored=False).order_by("created_date")[:1000]
        answer({"requests": [row(r) for r in reqs]})

    if action == "get":
        r = models.InviteRequest.objects.filter(id=int(rid)).first()
        answer({"request": row(r), "pending": is_open(r)} if r else {"request": None})

    with transaction.atomic():
        r = models.InviteRequest.objects.select_for_update().filter(id=int(rid)).first()
        if not r or not is_open(r):
            answer({"gone": True})
        if action == "approve":
            who = inviter()
            if who is None:
                answer({"error": "no inviter: set APROVA_AGENT_BOOKWYRM_INVITER"})
            r.invite = models.SiteInvite.objects.create(use_limit=1, user=who)
            r.save()
        else:
            r.ignored = True
            r.save()
    if action == "approve":
        # After the commit, as the admin page does: queued to celery.
        emailing.invite_email(r)
    answer({"ok": True})
except SystemExit:
    raise
except Exception as e:  # the detail goes to stderr, the agent's log
    print(type(e).__name__ + ": " + str(e), file=sys.stderr)
    answer({"error": type(e).__name__})
