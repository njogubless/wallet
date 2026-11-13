import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import 'app/app.dart';

/// Operations console (support/admin roles only). Build separately, e.g.
/// `flutter run -t lib/main_admin.dart -d chrome`. Authorization is enforced
/// by the API; this entry point only hides the console from other roles.
void main() {
  WidgetsFlutterBinding.ensureInitialized();
  runApp(const ProviderScope(child: WalletApp(admin: true)));
}
