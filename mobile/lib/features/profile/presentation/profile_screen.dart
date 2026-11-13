import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../../../app/providers.dart';
import '../../../app/theme/app_theme.dart';
import '../../../app/theme/tokens.dart';
import '../../../core/networking/api_error.dart';
import '../../../core/widgets/app_shell.dart';
import '../../../core/widgets/common.dart';
import '../../auth/presentation/session_controller.dart';

class ProfileScreen extends ConsumerWidget {
  const ProfileScreen({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final user = ref.watch(currentUserProvider);
    if (user == null) return const SizedBox.shrink();
    return Scaffold(
      appBar: const BrandHeader(title: 'Profile & Security'),
      body: ListView(padding: const EdgeInsets.all(AppSpace.md), children: [
        AppCard(
          child: Row(children: [
            CircleAvatar(radius: 28, backgroundColor: AppColors.primary, child: Text(user.firstName[0], style: AppText.headlineMd.copyWith(color: Colors.white))),
            const SizedBox(width: AppSpace.md),
            Expanded(
              child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                Text(user.fullName, style: AppText.headlineSm),
                Text(user.phone, style: AppText.bodyMd.copyWith(color: AppColors.secondary)),
                if (user.email != null) Text(user.email!, style: AppText.bodySm.copyWith(color: AppColors.muted)),
              ]),
            ),
          ]),
        ),
        const SizedBox(height: AppSpace.lg),
        const SectionLabel('Security'),
        _tile(context, Icons.pin_outlined, 'Change transaction PIN', 'Authorizes transfers and withdrawals', () => context.push('/profile/pin')),
        _tile(context, Icons.password_outlined, 'Change password', 'Signs out your other devices', () => context.push('/profile/password')),
        _tile(context, Icons.notifications_outlined, 'Notifications', 'Transaction and security alerts', () => context.push('/notifications')),
        const SizedBox(height: AppSpace.lg),
        const SectionLabel('Account'),
        _tile(context, Icons.speed_outlined, 'Limits & fees', 'Tier ${user.kycTier}', () => context.go('/wallet')),
        _tile(context, Icons.description_outlined, 'Statements', 'PDF and CSV', () => context.push('/statements')),
        const SizedBox(height: AppSpace.lg),
        OutlinedButton.icon(
          key: const Key('profile.logout'),
          onPressed: () => ref.read(sessionProvider.notifier).logout(),
          icon: const Icon(Icons.logout),
          label: const Text('Sign out'),
        ),
        const SizedBox(height: AppSpace.md),
        Text('We will never ask for your PIN or password by phone, SMS or email.',
            textAlign: TextAlign.center, style: AppText.bodySm.copyWith(color: AppColors.muted)),
      ]),
    );
  }

  Widget _tile(BuildContext context, IconData icon, String title, String sub, VoidCallback onTap) => Padding(
        padding: const EdgeInsets.only(bottom: AppSpace.sm),
        child: AppCard(
          onTap: onTap,
          child: Row(children: [
            Icon(icon, color: AppColors.primary),
            const SizedBox(width: AppSpace.md),
            Expanded(
              child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                Text(title, style: AppText.titleMd),
                Text(sub, style: AppText.bodySm.copyWith(color: AppColors.muted)),
              ]),
            ),
            const Icon(Icons.chevron_right, color: AppColors.muted),
          ]),
        ),
      );
}

class ChangePasswordScreen extends ConsumerStatefulWidget {
  const ChangePasswordScreen({super.key});
  @override
  ConsumerState<ChangePasswordScreen> createState() => _ChangePasswordScreenState();
}

class _ChangePasswordScreenState extends ConsumerState<ChangePasswordScreen> {
  final _current = TextEditingController();
  final _next = TextEditingController();
  String? _error;
  bool _busy = false;

  @override
  Widget build(BuildContext context) => Scaffold(
        appBar: AppBar(title: const Text('Change password')),
        body: ListView(padding: const EdgeInsets.all(AppSpace.md), children: [
          if (_error != null) Padding(padding: const EdgeInsets.only(bottom: AppSpace.md), child: Text(_error!, style: AppText.bodyMd.copyWith(color: AppColors.danger))),
          TextField(controller: _current, obscureText: true, decoration: const InputDecoration(labelText: 'Current password')),
          const SizedBox(height: AppSpace.md),
          TextField(controller: _next, obscureText: true, decoration: const InputDecoration(labelText: 'New password (min 10 characters)')),
          const SizedBox(height: AppSpace.lg),
          FilledButton(
            onPressed: _busy
                ? null
                : () async {
                    setState(() {
                      _busy = true;
                      _error = null;
                    });
                    try {
                      await ref.read(authRepositoryProvider).changePassword(_current.text, _next.text);
                      if (context.mounted) {
                        showSnack(context, 'Password changed');
                        context.pop();
                      }
                    } on ApiException catch (e) {
                      setState(() => _error = e.fields.values.firstOrNull ?? e.message);
                    } finally {
                      if (mounted) setState(() => _busy = false);
                    }
                  },
            child: const Text('Update password'),
          ),
        ]),
      );
}
