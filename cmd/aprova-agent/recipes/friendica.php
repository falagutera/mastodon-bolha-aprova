<?php
// aprova-agent recipe for Friendica's sign-ups awaiting approval (tested on 2026.05).
// Run by aprova-agent inside the Friendica image, with Friendica's own environment:
//
//   php friendica.php pending | get <uid> | approve <uid> | reject <uid>
//
// Prints one JSON object on its last line. Friendica is loaded the way
// bin/console loads it (a harmless "help" run sets up the services), then the
// same calls as `bin/console user allow|deny` decide. Approve sends Friendica's
// own welcome; deny deletes the pending account and sends nothing.

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
    answer($action === 'get' ? ['request' => null] : ['gone' => true]);
}

$root = getenv('APROVA_AGENT_FRIENDICA_ROOT') ?: '/var/www/html';
chdir($root);
require $root . '/vendor/autoload.php';
$container = \Friendica\Core\DiceContainer::fromBasePath($root);
$app = \Friendica\App::fromContainer($container);
// Sets up config, database and the legacy service locator, as bin/console does.
ob_start();
$app->processConsole(['argv' => ['console', 'help']]);
ob_end_clean();

use Friendica\Model\Register;
use Friendica\Model\User;

function row(array $p): array
{
    return [
        'id' => (string) $p['uid'],
        'username' => (string) ($p['nick'] ?? $p['nickname'] ?? ''),
        'email' => (string) ($p['email'] ?? ''),
        'reason' => (string) ($p['note'] ?? ''),
        'created' => !empty($p['created']) ? gmdate('c', strtotime($p['created'] . ' UTC')) : null,
    ];
}

if ($action === 'pending') {
    $out = [];
    foreach (Register::getPending(0, 1000) ?: [] as $p) {
        $out[] = row($p);
    }
    answer(['requests' => $out]);
}

$uid = (int) $id;
$pending = Register::getPendingForUser($uid);

if ($action === 'get') {
    if (!empty($pending)) {
        $u = User::getById($uid) ?: [];
        answer(['request' => row(array_merge($u, ['uid' => $uid, 'nick' => $u['nickname'] ?? '', 'email' => $u['email'] ?? '', 'note' => $pending['note'] ?? '', 'created' => $pending['created'] ?? ''])), 'pending' => true]);
    }
    $u = User::getById($uid);
    answer($u ? ['request' => row(array_merge($u, ['uid' => $uid])), 'pending' => false] : ['request' => null]);
}

if (empty($pending) || empty($pending['hash'])) {
    answer(['gone' => true]);
}
$ok = $action === 'approve' ? User::allow($pending['hash']) : User::deny($pending['hash']);
answer($ok ? ['ok' => true] : ['error' => 'friendica refused']);
