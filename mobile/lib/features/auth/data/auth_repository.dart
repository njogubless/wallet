import '../../../core/networking/api_client.dart';
import '../domain/user.dart';

abstract class AuthRepository {
  Future<AppUser> login(String phone, String password);
  Future<AppUser> register({required String phone, required String fullName, required String password, String? email});
  Future<AppUser?> restore();
  Future<AppUser> me();
  Future<void> logout();
  Future<AppUser> verifyPhone(String code);
  Future<void> resendCode();
  Future<void> setPin(String password, String pin);
  Future<void> changePassword(String current, String next);
  Future<void> forgotPassword(String phone);
  Future<void> resetPassword(String phone, String code, String newPassword);
}

class ApiAuthRepository implements AuthRepository {
  ApiAuthRepository(this.api);
  final ApiClient api;

  Future<AppUser> _session(Map<String, dynamic> res) async {
    await api.saveSession(res);
    return AppUser.fromJson((res['user'] as Map).cast());
  }

  @override
  Future<AppUser> login(String phone, String password) async =>
      _session(await api.post('/api/v1/auth/login', body: {'phone': phone, 'password': password}, auth: false));

  @override
  Future<AppUser> register({required String phone, required String fullName, required String password, String? email}) async =>
      _session(await api.post('/api/v1/auth/register',
          body: {'phone': phone, 'full_name': fullName, 'password': password, if (email != null && email.isNotEmpty) 'email': email},
          auth: false));

  @override
  Future<AppUser?> restore() async {
    if (await api.tokens.read() == null) return null;
    return me(); // refreshes transparently if the access token expired
  }

  @override
  Future<AppUser> me() async => AppUser.fromJson(await api.get('/api/v1/me'));

  @override
  Future<void> logout() async {
    final s = await api.tokens.read();
    try {
      if (s != null) await api.post('/api/v1/auth/logout', body: {'refresh_token': s.refreshToken}, auth: false);
    } finally {
      await api.tokens.clear();
    }
  }

  @override
  Future<AppUser> verifyPhone(String code) async => AppUser.fromJson(await api.post('/api/v1/auth/verify-phone', body: {'code': code}));

  @override
  Future<void> resendCode() => api.post('/api/v1/auth/verify-phone/resend');

  @override
  Future<void> setPin(String password, String pin) => api.post('/api/v1/me/pin', body: {'password': password, 'pin': pin});

  @override
  Future<void> changePassword(String current, String next) =>
      api.post('/api/v1/me/password', body: {'current_password': current, 'new_password': next});

  @override
  Future<void> forgotPassword(String phone) => api.post('/api/v1/auth/forgot-password', body: {'phone': phone}, auth: false);

  @override
  Future<void> resetPassword(String phone, String code, String newPassword) => api.post('/api/v1/auth/reset-password',
      body: {'phone': phone, 'code': code, 'new_password': newPassword}, auth: false);
}
