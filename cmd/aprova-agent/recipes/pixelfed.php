<?php
// aprova-agent recipe for Pixelfed's curated onboarding (tested on 0.15.2).
// Run by aprova-agent inside the Pixelfed image, with Pixelfed's own environment:
//
//   php pixelfed.php pending | get <id> | approve <id> | reject <id>
//
// Prints one JSON object on its last line. Approve and reject call Pixelfed's own
// admin controller, so its rules (the account it creates, the welcome e-mail)
// stay Pixelfed's. Reject is "reject-silent": aprova tells the applicant.

declare(strict_types=1);

function answer(array $v): void
{
    echo "\n", json_encode($v, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES), "\n";
    exit(0);
}

set_exception_handler(function (Throwable $e): void {
    fwrite(STDERR, get_class($e) . ': ' . $e->getMessage() . "\n");
    answer(['error' => get_class($e)]);
});

$action = $argv[1] ?? '';
$id = $argv[2] ?? '';
if (!in_array($action, ['pending', 'get', 'approve', 'reject'], true)) {
    answer(['error' => 'unknown action']);
}
if ($action !== 'pending' && !preg_match('/^[0-9]{1,19}$/', $id)) {
    // Not an id Pixelfed could have: nothing to find.
    answer($action === 'get' ? ['request' => null] : ['gone' => true]);
}

$root = getenv('APROVA_AGENT_PIXELFED_ROOT') ?: '/var/www/html';
require $root . '/vendor/autoload.php';
$app = require $root . '/bootstrap/app.php';
$app->make(Illuminate\Contracts\Console\Kernel::class)->bootstrap();

use App\Models\CuratedRegister;
use Illuminate\Http\Request;
use Illuminate\Support\Facades\DB;

// The admin panel's "open" filter: e-mail confirmed, not decided, not waiting on
// the applicant's answer. The panel refuses to act on unconfirmed e-mails.
function openQuery()
{
    return CuratedRegister::where('is_closed', false)
        ->where('is_rejected', false)
        ->whereNotNull('email_verified_at')
        ->where(function ($q) {
            $q->where('user_has_responded', true)->orWhere('is_awaiting_more_info', false);
        });
}

function isOpen(CuratedRegister $r): bool
{
    return !$r->is_closed && !$r->is_rejected && $r->email_verified_at !== null
        && ($r->user_has_responded || !$r->is_awaiting_more_info);
}

function row(CuratedRegister $r): array
{
    return [
        'id' => (string) $r->id,
        'username' => (string) $r->username,
        'email' => (string) $r->email,
        'reason' => (string) ($r->reason_to_join ?? ''),
        'created' => $r->created_at ? $r->created_at->toIso8601String() : null,
    ];
}

if ($action === 'pending') {
    answer(['requests' => openQuery()->orderBy('created_at')->limit(1000)->get()->map(fn ($r) => row($r))->all()]);
}

if ($action === 'get') {
    $r = CuratedRegister::find((int) $id);
    answer($r ? ['request' => row($r), 'pending' => isOpen($r)] : ['request' => null]);
}

// approve or reject, with the row locked so nothing else decides it meanwhile.
$controller = app(App\Http\Controllers\AdminCuratedRegisterController::class);
$done = DB::transaction(function () use ($action, $id, $controller) {
    $r = CuratedRegister::lockForUpdate()->find((int) $id);
    if (!$r || !isOpen($r)) {
        return false;
    }
    if ($action === 'approve') {
        $controller->apiHandleApprove(Request::create('/', 'POST'), $r->id);
    } else {
        $controller->apiHandleReject(Request::create('/', 'POST', ['action' => 'reject-silent']), $r->id);
    }
    return true;
});
answer($done ? ['ok' => true] : ['gone' => true]);
