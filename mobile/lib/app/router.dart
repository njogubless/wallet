import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../core/widgets/app_shell.dart';
import '../core/widgets/common.dart';
import '../features/admin/presentation/admin_screens.dart';
import '../features/auth/presentation/auth_screens.dart';
import '../features/auth/presentation/session_controller.dart';
import '../features/deposits/data/payments_repository.dart';
import '../features/deposits/presentation/deposit_screen.dart';
import '../features/deposits/presentation/payment_flow.dart';
import '../features/notifications/presentation/notifications_screen.dart';
import '../features/profile/presentation/profile_screen.dart';
import '../features/statements/presentation/statements_screen.dart';
import '../features/transactions/presentation/activity_screen.dart';
import '../features/transfers/presentation/transfer_screen.dart';
import '../features/wallet/presentation/home_screen.dart';
import '../features/withdrawals/presentation/withdraw_screen.dart';

/// Bridges Riverpod session changes to GoRouter's refreshListenable.
class _SessionListenable extends ChangeNotifier {
  _SessionListenable(Ref ref) {
    ref.listen(sessionProvider, (_, _) => notifyListeners());
  }
}

const _publicRoutes = {'/login', '/register', '/forgot-password'};

/// Central guard: unauthenticated users only see public routes; signed-in
/// users must verify their phone and set a PIN before reaching money flows.
String? guard(SessionState s, String location, {required bool admin}) {
  final public = _publicRoutes.contains(location);
  switch (s) {
    case SessionLoading():
      return location == '/splash' ? null : '/splash';
    case SignedOut():
      return public ? null : '/login';
    case SignedIn(:final user):
      if (admin) {
        if (!user.isStaff) return location == '/forbidden' ? null : '/forbidden';
        return location.startsWith('/admin') ? null : '/admin';
      }
      if (user.needsPhoneVerification) return location == '/verify-phone' ? null : '/verify-phone';
      if (!user.hasPin) return location == '/set-pin' ? null : '/set-pin';
      if (public || location == '/splash' || location == '/verify-phone' || location == '/set-pin') return '/home';
      return null;
  }
}

class SplashScreen extends StatelessWidget {
  const SplashScreen({super.key});
  @override
  Widget build(BuildContext context) => const Scaffold(body: LoadingView());
}

class _LoginRoute extends ConsumerWidget {
  const _LoginRoute();
  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final st = ref.watch(sessionProvider);
    return LoginScreen(message: st is SignedOut ? st.message : null);
  }
}

final routerProvider = Provider<GoRouter>((ref) {
  final listenable = _SessionListenable(ref);
  return GoRouter(
    initialLocation: '/splash',
    refreshListenable: listenable,
    redirect: (_, state) => guard(ref.read(sessionProvider), state.matchedLocation, admin: false),
    routes: [
      GoRoute(path: '/splash', builder: (_, _) => const SplashScreen()),
      GoRoute(path: '/login', builder: (_, _) => const _LoginRoute()),
      GoRoute(path: '/register', builder: (_, _) => const RegisterScreen()),
      GoRoute(path: '/forgot-password', builder: (_, _) => const ForgotPasswordScreen()),
      GoRoute(path: '/verify-phone', builder: (_, _) => const VerifyPhoneScreen()),
      GoRoute(path: '/set-pin', builder: (_, _) => const SetPinScreen()),
      StatefulShellRoute.indexedStack(
        builder: (_, _, shell) => AppShell(shell: shell),
        branches: [
          StatefulShellBranch(routes: [GoRoute(path: '/home', builder: (_, _) => const HomeScreen())]),
          StatefulShellBranch(routes: [GoRoute(path: '/wallet', builder: (_, _) => const WalletScreen())]),
          StatefulShellBranch(routes: [GoRoute(path: '/transfer', builder: (_, _) => const TransferScreen())]),
          StatefulShellBranch(routes: [GoRoute(path: '/activity', builder: (_, _) => const ActivityScreen())]),
          StatefulShellBranch(routes: [GoRoute(path: '/profile', builder: (_, _) => const ProfileScreen())]),
        ],
      ),
      GoRoute(path: '/deposit', builder: (_, _) => const DepositScreen()),
      GoRoute(path: '/withdraw', builder: (_, _) => const WithdrawScreen()),
      GoRoute(path: '/transfer/done', builder: (_, s) => TransferDoneScreen(result: s.extra! as TransferResult)),
      GoRoute(path: '/payments/:kind/:id', builder: (_, s) => PaymentStatusScreen(kind: s.pathParameters['kind']!, id: s.pathParameters['id']!)),
      GoRoute(path: '/transactions/:id', builder: (_, s) => TransactionDetailScreen(id: s.pathParameters['id']!)),
      GoRoute(path: '/statements', builder: (_, _) => const StatementsScreen()),
      GoRoute(path: '/notifications', builder: (_, _) => const NotificationsScreen()),
      GoRoute(path: '/profile/pin', builder: (_, _) => const SetPinScreen(isChange: true)),
      GoRoute(path: '/profile/password', builder: (_, _) => const ChangePasswordScreen()),
    ],
  );
});

class ForbiddenScreen extends ConsumerWidget {
  const ForbiddenScreen({super.key});
  @override
  Widget build(BuildContext context, WidgetRef ref) => Scaffold(
        body: Center(
          child: Column(mainAxisSize: MainAxisSize.min, children: [
            const Icon(Icons.lock_outline, size: 48),
            const SizedBox(height: 12),
            const Text('This console is restricted to operations staff.'),
            const SizedBox(height: 12),
            OutlinedButton(onPressed: () => ref.read(sessionProvider.notifier).logout(), child: const Text('Sign out')),
          ]),
        ),
      );
}

final adminRouterProvider = Provider<GoRouter>((ref) {
  final listenable = _SessionListenable(ref);
  Widget shell(GoRouterState s, Widget child) => AdminShell(location: s.matchedLocation, child: child);
  return GoRouter(
    initialLocation: '/splash',
    refreshListenable: listenable,
    redirect: (_, state) => guard(ref.read(sessionProvider), state.matchedLocation, admin: true),
    routes: [
      GoRoute(path: '/splash', builder: (_, _) => const SplashScreen()),
      GoRoute(path: '/login', builder: (_, _) => const _LoginRoute()),
      GoRoute(path: '/forgot-password', builder: (_, _) => const ForgotPasswordScreen()),
      GoRoute(path: '/forbidden', builder: (_, _) => const ForbiddenScreen()),
      GoRoute(path: '/admin', builder: (_, s) => shell(s, const AdminDashboardScreen())),
      GoRoute(path: '/admin/payments', builder: (_, s) => shell(s, const AdminPaymentsScreen())),
      GoRoute(path: '/admin/reconciliation', builder: (_, s) => shell(s, const AdminReconciliationScreen())),
      GoRoute(path: '/admin/approvals', builder: (_, s) => shell(s, const AdminApprovalsScreen())),
      GoRoute(path: '/admin/users', builder: (_, s) => shell(s, const AdminUsersScreen())),
      GoRoute(path: '/admin/users/:id', builder: (_, s) => shell(s, AdminUserDetailScreen(id: s.pathParameters['id']!))),
      GoRoute(path: '/admin/audit', builder: (_, s) => shell(s, const AdminAuditScreen())),
    ],
  );
});
