import 'dart:convert';
import 'dart:typed_data';

import 'package:dio/dio.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:sovereign_wallet/core/money/money.dart';
import 'package:sovereign_wallet/core/networking/api_client.dart';
import 'package:sovereign_wallet/core/networking/api_error.dart';
import 'package:sovereign_wallet/core/networking/idempotency.dart';
import 'package:sovereign_wallet/core/security/token_store.dart';
import 'package:sovereign_wallet/core/widgets/money_widgets.dart';
import 'package:sovereign_wallet/features/deposits/presentation/payment_flow.dart';

void main() {
  group('Money', () {
    test('parses exact decimal strings into minor units', () {
      expect(Money.parseMinor('5000', Currency.kes), 500000);
      expect(Money.parseMinor('5000.5', Currency.kes), 500050);
      expect(Money.parseMinor('0.01', Currency.kes), 1);
    });

    test('rejects malformed or over-precise input', () {
      for (final bad in ['', '-1', '1e3', '1,000', '5.001', 'abc', '.5', '1.']) {
        expect(() => Money.parseMinor(bad, Currency.kes), throwsA(isA<MoneyFormatException>()), reason: bad);
      }
    });

    test('formats with grouping and KES prefix', () {
      final m = const Money(8425000, Currency.kes);
      expect(m.formatNumber(), '84,250.00');
      expect(m.format(), 'KES 84,250.00');
      expect(m.toWire(), '84250.00');
      expect(const Money(-150, Currency.kes).toWire(), '-1.50');
      expect(const Money(100, Currency.kes).formatNumber(), '1.00');
      expect(const Money(123456789012, Currency.kes).formatNumber(), '1,234,567,890.12');
    });

    test('round-trips the wire format without floating point', () {
      for (final s in ['0.10', '0.20', '0.30', '1234567.89']) {
        final m = Money.fromJson({'amount': s, 'currency': 'KES'});
        expect(m.toWire(), s);
      }
      // 0.1 + 0.2 is exact in minor units.
      expect((Money.fromJson({'amount': '0.10', 'currency': 'KES'}) + Money.fromJson({'amount': '0.20', 'currency': 'KES'})).toWire(), '0.30');
    });

    test('refuses to mix currencies', () {
      expect(() => const Money(1, Currency.kes) + const Money(1, Currency('USD', 2)), throwsArgumentError);
    });
  });

  group('MoneyInput.validate', () {
    test('enforces format, whole units and limits', () {
      expect(MoneyInput.validate(''), 'Enter an amount');
      expect(MoneyInput.validate('0'), 'Amount must be greater than zero');
      expect(MoneyInput.validate('10.5', wholeUnitsOnly: true), 'Enter whole shillings only');
      expect(MoneyInput.validate('5', min: const Money(1000, Currency.kes)), contains('Minimum'));
      expect(MoneyInput.validate('200000', max: const Money(15000000, Currency.kes)), contains('Maximum'));
      expect(MoneyInput.validate('1,500'), isNull);
      expect(MoneyInput.validate('10.555'), isNotNull);
    });
  });

  group('Idempotency', () {
    test('same parameters reuse the key, changed parameters renew it', () {
      final a = PaymentAttempt.forRequest({'amount': '100', 'to': 'x'});
      expect(a.reuseOrRenew({'to': 'x', 'amount': '100'}).key, a.key);
      expect(a.reuseOrRenew({'to': 'x', 'amount': '101'}).key, isNot(a.key));
    });

    test('submitter reuses the key after an unknown outcome and blocks concurrent submits', () async {
      final s = PaymentSubmitter();
      final keys = <String>[];
      final first = await s.submit({'amount': '1'}, (a) async {
        keys.add(a.key);
        throw ApiException(code: 'timeout', message: 't', outcomeUnknown: true);
      });
      expect(first, isA<OutcomeUnknown>());
      final second = await s.submit({'amount': '1'}, (a) async {
        keys.add(a.key);
        return 'ok';
      });
      expect(second, isA<Submitted>());
      expect(keys.toSet().length, 1, reason: 'retry must reuse the idempotency key');

      final slow = s.submit({'amount': '2'}, (a) => Future.delayed(const Duration(milliseconds: 20), () => 'x'));
      expect(await s.submit({'amount': '2'}, (a) async => 'y'), isNull, reason: 'concurrent submit ignored');
      expect(await slow, isA<Submitted>());
    });

    test('definitive rejection is not reported as unknown', () async {
      final r = await PaymentSubmitter().submit({'a': 1}, (_) async => throw ApiException(code: 'insufficient_funds', message: 'no', status: 422));
      expect(r, isA<Rejected>());
    });
  });

  group('ApiClient session handling', () {
    late MemoryTokenStore store;
    late _ScriptedAdapter adapter;
    late ApiClient client;
    var expired = 0;

    setUp(() {
      store = MemoryTokenStore()
        ..session = StoredSession(accessToken: 'old', refreshToken: 'r1', accessExpiresAt: DateTime.utc(2026));
      adapter = _ScriptedAdapter();
      final dio = Dio(BaseOptions(baseUrl: 'http://test'))..httpClientAdapter = adapter;
      expired = 0;
      client = ApiClient(baseUrl: 'http://test', tokens: store, dio: dio, onSessionExpired: () => expired++);
    });

    test('refreshes once on 401 and retries with the new token', () async {
      adapter.handler = (o) {
        if (o.path.endsWith('/auth/refresh')) {
          return _json(200, {'access_token': 'new', 'refresh_token': 'r2', 'access_expires_at': '2030-01-01T00:00:00Z'});
        }
        return o.headers['Authorization'] == 'Bearer new' ? _json(200, {'ok': true}) : _json(401, {'error': {'code': 'unauthenticated', 'message': 'x'}});
      };
      final results = await Future.wait([client.get('/a'), client.get('/b'), client.get('/c')]);
      expect(results.every((r) => r['ok'] == true), isTrue);
      expect(adapter.calls.where((p) => p.endsWith('/auth/refresh')).length, 1, reason: 'single-flight refresh');
      expect((await store.read())!.refreshToken, 'r2');
    });

    test('clears session and signals expiry when refresh fails', () async {
      adapter.handler = (o) => _json(401, {'error': {'code': 'unauthenticated', 'message': 'expired'}});
      await expectLater(client.get('/a'), throwsA(isA<ApiException>().having((e) => e.code, 'code', 'unauthenticated')));
      expect(await store.read(), isNull);
      expect(expired, 1);
    });

    test('maps server errors and timeouts', () async {
      adapter.handler = (o) => _json(422, {'error': {'code': 'insufficient_funds', 'message': 'Insufficient', 'request_id': 'rq'}});
      await expectLater(client.post('/x'), throwsA(isA<ApiException>().having((e) => e.outcomeUnknown, 'unknown', false).having((e) => e.code, 'code', 'insufficient_funds')));
      adapter.handler = (o) => throw DioException(requestOptions: o, type: DioExceptionType.receiveTimeout);
      await expectLater(client.post('/x'), throwsA(isA<ApiException>().having((e) => e.outcomeUnknown, 'unknown', true)));
    });

    test('sends the idempotency key header', () async {
      adapter.handler = (o) => _json(200, {'idem': o.headers['Idempotency-Key']});
      expect((await client.post('/x', idempotencyKey: 'k-123'))['idem'], 'k-123');
    });
  });
}

ResponseBody _json(int status, Object body) =>
    ResponseBody.fromString(jsonEncode(body), status, headers: {'content-type': ['application/json']});

class _ScriptedAdapter implements HttpClientAdapter {
  late ResponseBody Function(RequestOptions) handler;
  final calls = <String>[];

  @override
  Future<ResponseBody> fetch(RequestOptions options, Stream<Uint8List>? requestStream, Future<void>? cancelFuture) async {
    calls.add(options.path);
    await Future<void>.delayed(const Duration(milliseconds: 5));
    return handler(options);
  }

  @override
  void close({bool force = false}) {}
}
