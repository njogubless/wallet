/// Build-time configuration. Pass with --dart-define, e.g.
/// `flutter run --dart-define=API_BASE_URL=https://api.example.com`.
/// No secrets belong here: everything in the app binary is public.
class AppConfig {
  const AppConfig({required this.apiBaseUrl});

  final String apiBaseUrl;

  static const fromEnvironment = AppConfig(
    apiBaseUrl: String.fromEnvironment('API_BASE_URL', defaultValue: 'http://10.0.2.2:8080'),
  );
}
