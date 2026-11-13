import 'package:flutter_secure_storage/flutter_secure_storage.dart';

class StoredSession {
  const StoredSession({required this.accessToken, required this.refreshToken, required this.accessExpiresAt});
  final String accessToken;
  final String refreshToken;
  final DateTime accessExpiresAt;
}

/// Persists tokens. Implementations must never log token values.
abstract class TokenStore {
  Future<StoredSession?> read();
  Future<void> write(StoredSession session);
  Future<void> clear();
}

/// Keychain (iOS) / Keystore-backed encrypted storage (Android).
class SecureTokenStore implements TokenStore {
  SecureTokenStore([FlutterSecureStorage? storage]) : _s = storage ?? const FlutterSecureStorage();
  final FlutterSecureStorage _s;

  static const _access = 'access_token';
  static const _refresh = 'refresh_token';
  static const _exp = 'access_expires_at';

  @override
  Future<StoredSession?> read() async {
    final a = await _s.read(key: _access);
    final r = await _s.read(key: _refresh);
    final e = await _s.read(key: _exp);
    if (a == null || r == null || e == null) return null;
    return StoredSession(accessToken: a, refreshToken: r, accessExpiresAt: DateTime.tryParse(e) ?? DateTime(0));
  }

  @override
  Future<void> write(StoredSession session) async {
    await _s.write(key: _access, value: session.accessToken);
    await _s.write(key: _refresh, value: session.refreshToken);
    await _s.write(key: _exp, value: session.accessExpiresAt.toIso8601String());
  }

  @override
  Future<void> clear() async {
    await _s.delete(key: _access);
    await _s.delete(key: _refresh);
    await _s.delete(key: _exp);
  }
}

/// In-memory store for tests.
class MemoryTokenStore implements TokenStore {
  StoredSession? session;
  @override
  Future<void> clear() async => session = null;
  @override
  Future<StoredSession?> read() async => session;
  @override
  Future<void> write(StoredSession s) async => session = s;
}
