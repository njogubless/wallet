import 'dart:async';

import 'package:dio/dio.dart';

import '../security/token_store.dart';
import 'api_error.dart';

/// HTTP client for the wallet API.
///
/// - Attaches the access token; on 401 performs a single-flight refresh and
///   retries the request once. If refresh fails the session is cleared and
///   [onSessionExpired] fires.
/// - Never logs request bodies or headers (they carry tokens and PINs).
class ApiClient {
  ApiClient({required String baseUrl, required this.tokens, Dio? dio, this.onSessionExpired})
      : dio = dio ??
            Dio(BaseOptions(
              baseUrl: baseUrl,
              connectTimeout: const Duration(seconds: 10),
              sendTimeout: const Duration(seconds: 20),
              receiveTimeout: const Duration(seconds: 30),
              contentType: 'application/json',
              responseType: ResponseType.json,
            )) {
    this.dio.interceptors.add(InterceptorsWrapper(onRequest: _onRequest, onError: _onError));
  }

  final Dio dio;
  final TokenStore tokens;
  void Function()? onSessionExpired;

  Completer<bool>? _refreshing;

  static const _skipAuth = 'skipAuth';
  static const _retried = 'retried';

  Future<void> _onRequest(RequestOptions o, RequestInterceptorHandler h) async {
    if (o.extra[_skipAuth] != true) {
      final s = await tokens.read();
      if (s != null) o.headers['Authorization'] = 'Bearer ${s.accessToken}';
    }
    h.next(o);
  }

  Future<void> _onError(DioException e, ErrorInterceptorHandler h) async {
    final o = e.requestOptions;
    if (e.response?.statusCode == 401 && o.extra[_skipAuth] != true && o.extra[_retried] != true) {
      final ok = await _refresh();
      if (ok) {
        try {
          final s = await tokens.read();
          o.headers['Authorization'] = 'Bearer ${s!.accessToken}';
          o.extra[_retried] = true;
          return h.resolve(await dio.fetch(o));
        } on DioException catch (err) {
          return h.next(err);
        }
      }
    }
    h.next(e);
  }

  /// Refreshes tokens once even if many requests fail concurrently.
  Future<bool> _refresh() async {
    if (_refreshing != null) return _refreshing!.future;
    final c = _refreshing = Completer<bool>();
    try {
      final s = await tokens.read();
      if (s == null) {
        c.complete(false);
      } else {
        final res = await dio.post<Map<String, dynamic>>('/api/v1/auth/refresh',
            data: {'refresh_token': s.refreshToken}, options: Options(extra: {_skipAuth: true}));
        await saveSession(res.data!);
        c.complete(true);
      }
    } catch (_) {
      await tokens.clear();
      c.complete(false);
      onSessionExpired?.call();
    } finally {
      _refreshing = null;
    }
    return c.future;
  }

  Future<void> saveSession(Map<String, dynamic> json) => tokens.write(StoredSession(
        accessToken: json['access_token'] as String,
        refreshToken: json['refresh_token'] as String,
        accessExpiresAt: DateTime.parse(json['access_expires_at'] as String),
      ));

  Future<T> _wrap<T>(Future<Response<dynamic>> Function() call, T Function(dynamic) parse) async {
    try {
      final res = await call();
      return parse(res.data);
    } on DioException catch (e) {
      throw ApiException.fromDio(e);
    }
  }

  Future<Map<String, dynamic>> get(String path, {Map<String, dynamic>? query}) =>
      _wrap(() => dio.get(path, queryParameters: query), (d) => (d as Map).cast<String, dynamic>());

  Future<Map<String, dynamic>> post(String path,
          {Object? body, String? idempotencyKey, bool auth = true}) =>
      _wrap(
        () => dio.post(path,
            data: body ?? const <String, dynamic>{},
            options: Options(
              headers: {'Idempotency-Key': ?idempotencyKey},
              extra: {if (!auth) _skipAuth: true},
            )),
        (d) => d is Map ? d.cast<String, dynamic>() : <String, dynamic>{},
      );

  Future<List<int>> getBytes(String path) => _wrap(
        () => dio.get<List<int>>(path, options: Options(responseType: ResponseType.bytes)),
        (d) => d as List<int>,
      );
}
