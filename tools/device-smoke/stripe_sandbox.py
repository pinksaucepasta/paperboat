#!/usr/bin/env python3
"""The Stripe SANDBOX actions the billing rows need and no product screen offers. It runs ON the host that already holds the
rehearsal's sandbox key (billing_checks.py sends this file over ssh stdin: `python3 -I - KEY_FILE ACTION EMAIL [ARGS]`), so the key
is read there and never leaves: it is not printed, copied or written, and an error text has any key-like string masked.

Fences, all checked before anything changes: the key is a test-mode one (sk_test_ / rk_test_); the only customer touched is the one
whose e-mail is a throwaway account of this harness (final-<10 hex>@), exactly one, made in the last six hours, not live mode.

  renewal-fail  EMAIL      the customer's card becomes one that fails when charged, and the subscription gets a trial ending in about
                           a minute: Stripe then bills it like a renewal (billing_reason subscription_cycle). The invoice is
                           finalized at once and paid, which is declined.
  retry         EMAIL INVOICE   one more payment attempt on that invoice (declined)
  recover       EMAIL INVOICE   a good card, then the invoice is paid
  refund        EMAIL CENTS     refunds the one paid charge of exactly CENTS
  cleanup       EMAIL PM...     detaches the payment methods renewal-fail / recover attached
Prints one JSON line (ids, statuses, amounts, times); exit 1 with {"error": ...} when a fence or a Stripe call fails.
"""
import json
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

THROWAWAY = re.compile(r'final-[0-9a-f]{10}@[A-Za-z0-9.-]+')
TEST_KEY = re.compile(r'(sk|rk)_test_[A-Za-z0-9]+')
API = 'https://api.stripe.com'


class Refused(Exception):
    pass


def mask(text):
    return re.sub(r'(sk|rk|whsec)_[A-Za-z0-9_*]+', r'\1_<masked>', str(text))


def read_key(path):
    with open(path) as handle:
        key = handle.read().strip()
    if not TEST_KEY.fullmatch(key):
        raise Refused('the key file does not hold a Stripe TEST-mode secret or restricted key: nothing was done')
    return key


def client(key):
    def call(method, path, fields=None, expect=(200,)):
        data = urllib.parse.urlencode(fields or {}).encode() if method != 'GET' else None
        url = API + path + ('?' + urllib.parse.urlencode(fields) if method == 'GET' and fields else '')
        request = urllib.request.Request(url, data=data, method=method, headers={'Authorization': 'Bearer ' + key})
        try:
            with urllib.request.urlopen(request, timeout=60) as response:
                status, body = response.status, json.loads(response.read())
        except urllib.error.HTTPError as error:
            status, body = error.code, json.loads(error.read() or b'{}')
        if status not in expect:
            problem = body.get('error') or {}
            raise Refused(mask('%s %s answered %d %s: %s' % (method, path, status, problem.get('code'), problem.get('message'))))
        return status, body

    return call


def throwaway_customer(call, email, now=None):
    if not THROWAWAY.fullmatch(email):
        raise Refused('only a throwaway account of this harness (final-<hex>@) may be touched')
    found = call('GET', '/v1/customers', {'email': email, 'limit': 3})[1]['data']
    if len(found) != 1:
        raise Refused('expected exactly one Stripe customer for the throwaway account, found %d' % len(found))
    customer = found[0]
    if customer.get('livemode') is not False or customer.get('email') != email or customer.get('created', 0) < (now or time.time()) - 6 * 3600:
        raise Refused('the customer is not a fresh test-mode one: nothing was done')
    return customer


def subscription(call, customer):
    found = [s for s in call('GET', '/v1/subscriptions', {'customer': customer['id'], 'status': 'all', 'limit': 10})[1]['data'] if s['status'] != 'canceled']
    if len(found) != 1:
        raise Refused('expected one subscription, found %d' % len(found))
    return found[0]


def invoice_of(call, customer, invoice_id):
    if not re.fullmatch(r'in_[A-Za-z0-9]+', invoice_id):
        raise Refused('bad invoice id')
    invoice = call('GET', '/v1/invoices/' + invoice_id)[1]
    if invoice.get('customer') != customer['id']:
        raise Refused('the invoice belongs to another customer')
    return invoice


def use_card(call, customer, sub, test_method):
    method = call('POST', '/v1/payment_methods/%s/attach' % test_method, {'customer': customer['id']})[1]['id']
    call('POST', '/v1/customers/' + customer['id'], {'invoice_settings[default_payment_method]': method})
    call('POST', '/v1/subscriptions/' + sub['id'], {'default_payment_method': method})
    return method


def view(call, invoice_id, sub_id):
    invoice, sub = call('GET', '/v1/invoices/' + invoice_id)[1], call('GET', '/v1/subscriptions/' + sub_id)[1]
    return {'invoice': invoice['id'], 'billing_reason': invoice.get('billing_reason'), 'invoice_status': invoice.get('status'),
            'attempt_count': invoice.get('attempt_count'), 'amount_due': invoice.get('amount_due'), 'amount_paid': invoice.get('amount_paid'),
            'finalized_at': (invoice.get('status_transitions') or {}).get('finalized_at'), 'subscription_status': sub['status']}


def settle(call, invoice_id, sub_id, done, seconds=90):
    deadline = time.time() + seconds
    while True:
        seen = view(call, invoice_id, sub_id)
        if done(seen) or time.time() > deadline:
            return seen
        time.sleep(3)


def pay(call, invoice_id):
    """One payment attempt; (HTTP status, decline code or None)."""
    status, body = call('POST', '/v1/invoices/%s/pay' % invoice_id, expect=(200, 402))
    return status, None if status == 200 else (body.get('error') or {}).get('code')


def renewal_fail(call, customer, wait=75):
    sub = subscription(call, customer)
    before = sub.get('latest_invoice')
    method = use_card(call, customer, sub, 'pm_card_chargeCustomerFail')
    call('POST', '/v1/subscriptions/' + sub['id'], {'trial_end': int(time.time()) + wait, 'proration_behavior': 'none'})
    deadline, invoice = time.time() + wait + 600, None
    while time.time() < deadline and not invoice:
        time.sleep(10)
        listed = call('GET', '/v1/invoices', {'subscription': sub['id'], 'limit': 10})[1]['data']
        invoice = next((i for i in listed if i['id'] != before and i.get('billing_reason') == 'subscription_cycle' and i.get('amount_due', 0) > 0), None)
    if not invoice:
        raise Refused('no renewal invoice appeared after the trial ended')
    if invoice['status'] == 'draft':  # Stripe would finalize it about an hour later on its own
        call('POST', '/v1/invoices/%s/finalize' % invoice['id'], {'auto_advance': 'false'})
    attempted_at = int(time.time())
    status, decline = (402, 'already attempted') if invoice.get('attempted') else pay(call, invoice['id'])
    seen = settle(call, invoice['id'], sub['id'], lambda v: v['subscription_status'] == 'past_due')
    return dict(seen, payment_status=status, decline_code=decline, attempted_at=attempted_at, payment_method=method)


def retry(call, customer, invoice_id):
    sub = subscription(call, customer)
    invoice_of(call, customer, invoice_id)
    attempted_at = int(time.time())
    status, decline = pay(call, invoice_id)
    return dict(view(call, invoice_id, sub['id']), payment_status=status, decline_code=decline, attempted_at=attempted_at,
                webhook_delivered=delivered(call, invoice_id, attempted_at))


def delivered(call, invoice_id, since, seconds=180):
    """Whether Stripe says every webhook endpoint has taken the payment_failed event of the attempt made at `since`. A check that asserts
    nothing changed must not read before that."""
    deadline = time.time() + seconds
    while True:
        events = call('GET', '/v1/events', {'type': 'invoice.payment_failed', 'created[gte]': int(since) - 5, 'limit': 50})[1]['data']
        mine = [e for e in events if e['data']['object'].get('id') == invoice_id]
        if mine and not any(e.get('pending_webhooks') for e in mine):
            return True
        if time.time() > deadline:
            return False  # another endpoint of the sandbox may stay pending: by now the server's had its time too
        time.sleep(5)


def recover(call, customer, invoice_id):
    sub = subscription(call, customer)
    invoice_of(call, customer, invoice_id)
    method = use_card(call, customer, sub, 'pm_card_visa')
    status, decline = pay(call, invoice_id)
    seen = settle(call, invoice_id, sub['id'], lambda v: v['subscription_status'] == 'active')
    return dict(seen, payment_status=status, decline_code=decline, payment_method=method)


def refund(call, customer, cents):
    charges = call('GET', '/v1/charges', {'customer': customer['id'], 'limit': 20})[1]['data']
    paid = [c for c in charges if c.get('status') == 'succeeded' and c.get('amount') == cents and not c.get('refunded') and c.get('livemode') is False]
    if len(paid) != 1:
        raise Refused('expected one paid, unrefunded charge of %d cents, found %d' % (cents, len(paid)))
    made = call('POST', '/v1/refunds', {'charge': paid[0]['id']})[1]
    after = call('GET', '/v1/charges/' + paid[0]['id'])[1]
    return {'charge': paid[0]['id'], 'refund': made['id'], 'refund_status': made['status'], 'refund_amount': made['amount'],
            'charge_refunded': after.get('refunded'), 'amount_refunded': after.get('amount_refunded')}


def cleanup(call, customer, methods):
    detached, failed = [], []
    for method in methods:
        try:  # one method Stripe refuses must not leave the others attached
            found = call('GET', '/v1/payment_methods/' + method)[1] if re.fullmatch(r'pm_[A-Za-z0-9]+', method) else {}
            if found.get('customer') == customer['id']:
                call('POST', '/v1/payment_methods/%s/detach' % method)
                detached.append(method)
        except Refused as error:
            failed.append(mask(error))
    return {'detached': len(detached), 'failed': failed}


def main(argv):
    try:
        if len(argv) < 3:
            raise Refused(__doc__)
        path, action, email, rest = argv[0], argv[1], argv[2], argv[3:]
        call = client(read_key(path))
        customer = throwaway_customer(call, email)
        if action == 'renewal-fail':
            out = renewal_fail(call, customer)
        elif action == 'retry':
            out = retry(call, customer, rest[0])
        elif action == 'recover':
            out = recover(call, customer, rest[0])
        elif action == 'refund':
            out = refund(call, customer, int(rest[0]))
        elif action == 'cleanup':
            out = cleanup(call, customer, rest)
        else:
            raise Refused('unknown action ' + action)
    except Exception as error:  # noqa: BLE001 every failure is one masked line, never a traceback that could quote a request
        print(json.dumps({'error': mask('%s: %s' % (type(error).__name__, error))[:600]}))
        return 1
    print(json.dumps(out))
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
