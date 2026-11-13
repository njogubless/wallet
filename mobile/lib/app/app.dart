import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import 'router.dart';
import 'theme/app_theme.dart';

class WalletApp extends ConsumerWidget {
  const WalletApp({super.key, this.admin = false});
  final bool admin;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final GoRouter router = ref.watch(admin ? adminRouterProvider : routerProvider);
    return MaterialApp.router(
      title: admin ? 'Wallet Operations' : 'Sovereign Trust Wallet',
      debugShowCheckedModeBanner: false,
      theme: buildTheme(),
      routerConfig: router,
    );
  }
}
