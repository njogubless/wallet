import 'package:uuid/uuid.dart';

/// Tracks the idempotency key for one logical money-moving request.
///
/// The same key is reused for every retry of the same request (same
/// parameters), so a retry after a timeout can never execute twice. Changing
/// any parameter, or starting a new payment, yields a new key.
class PaymentAttempt {
  PaymentAttempt._(this.fingerprint, this.key);

  final String fingerprint;
  final String key;

  static const _uuid = Uuid();

  factory PaymentAttempt.forRequest(Map<String, Object?> params) =>
      PaymentAttempt._(_fingerprint(params), _uuid.v4());

  /// Returns this attempt if [params] are unchanged, otherwise a new one.
  PaymentAttempt reuseOrRenew(Map<String, Object?> params) {
    final fp = _fingerprint(params);
    return fp == fingerprint ? this : PaymentAttempt._(fp, _uuid.v4());
  }

  static String _fingerprint(Map<String, Object?> params) {
    final keys = params.keys.toList()..sort();
    return keys.map((k) => '$k=${params[k]}').join('&');
  }
}
