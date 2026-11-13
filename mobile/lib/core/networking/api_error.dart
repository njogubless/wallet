import 'package:dio/dio.dart';

/// Normalized API failure.
class ApiException implements Exception {
  ApiException({
    required this.code,
    required this.message,
    this.status,
    this.fields = const {},
    this.requestId,
    this.outcomeUnknown = false,
  });

  /// Server error code (e.g. `insufficient_funds`) or a client-side code:
  /// `network_error` (request may not have reached the server) and
  /// `timeout` (request may have been processed - outcome unknown).
  final String code;
  final String message;
  final int? status;
  final Map<String, String> fields;
  final String? requestId;

  /// True when a money-moving request may or may not have been executed.
  /// The UI must offer "check status" (retry with the same idempotency key)
  /// rather than telling the user it failed.
  final bool outcomeUnknown;

  bool get isUnauthenticated => code == 'unauthenticated';

  factory ApiException.fromDio(DioException e) {
    final res = e.response;
    final data = res?.data;
    if (data is Map && data['error'] is Map) {
      final err = data['error'] as Map;
      return ApiException(
        code: '${err['code']}',
        message: '${err['message']}',
        status: res?.statusCode,
        fields: (err['fields'] as Map?)?.map((k, v) => MapEntry('$k', '$v')) ?? const {},
        requestId: err['request_id'] as String?,
        outcomeUnknown: (res?.statusCode ?? 0) >= 500,
      );
    }
    switch (e.type) {
      case DioExceptionType.connectionError:
        return ApiException(code: 'network_error', message: 'No connection. Check your internet and try again.');
      case DioExceptionType.connectionTimeout:
        return ApiException(code: 'network_error', message: 'Could not reach the server. Try again.');
      case DioExceptionType.sendTimeout:
      case DioExceptionType.receiveTimeout:
        return ApiException(
          code: 'timeout',
          message: 'The server did not respond in time. Your request may still be processing.',
          outcomeUnknown: true,
        );
      default:
        return ApiException(
          code: 'unexpected',
          message: 'Something went wrong. Please try again.',
          status: res?.statusCode,
          outcomeUnknown: (res?.statusCode ?? 0) >= 500 || res == null,
        );
    }
  }

  @override
  String toString() => 'ApiException($code, $status)';
}
