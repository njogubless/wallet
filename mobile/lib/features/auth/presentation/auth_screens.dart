import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../../../app/providers.dart';
import '../../../app/theme/app_theme.dart';
import '../../../app/theme/tokens.dart';
import '../../../core/networking/api_error.dart';
import '../../../core/widgets/common.dart';
import 'session_controller.dart';

class _AuthScaffold extends StatelessWidget {
  const _AuthScaffold({required this.title, required this.subtitle, required this.children});
  final String title;
  final String subtitle;
  final List<Widget> children;

  @override
  Widget build(BuildContext context) => Scaffold(
        body: SafeArea(
          child: Center(
            child: ConstrainedBox(
              constraints: const BoxConstraints(maxWidth: 480),
              child: ListView(padding: const EdgeInsets.all(AppSpace.lg), children: [
                const SizedBox(height: AppSpace.lg),
                Row(children: [
                  Container(
                    width: 44,
                    height: 44,
                    decoration: BoxDecoration(color: AppColors.primary, borderRadius: BorderRadius.circular(AppRadius.control)),
                    child: const Icon(Icons.shield_outlined, color: AppColors.tertiaryAccent),
                  ),
                  const SizedBox(width: AppSpace.md),
                  Text('SOVEREIGN TRUST', style: AppText.labelLg.copyWith(color: AppColors.secondary, letterSpacing: 1.2)),
                ]),
                const SizedBox(height: AppSpace.xl),
                Text(title, style: AppText.headlineLg),
                const SizedBox(height: AppSpace.xs),
                Text(subtitle, style: AppText.bodyMd.copyWith(color: AppColors.muted)),
                const SizedBox(height: AppSpace.lg),
                ...children,
              ]),
            ),
          ),
        ),
      );
}

String? _required(String? v, String name) => (v == null || v.trim().isEmpty) ? 'Enter your $name' : null;

String? validateKenyanPhone(String? v) {
  final s = (v ?? '').replaceAll(RegExp(r'[\s-]'), '');
  if (s.isEmpty) return 'Enter your phone number';
  if (!RegExp(r'^(?:\+?254|0)?[71]\d{8}$').hasMatch(s)) return 'Enter a valid Kenyan mobile number';
  return null;
}

/// Handles submit state and surfaces server field errors.
mixin _Submitting<T extends StatefulWidget> on State<T> {
  bool busy = false;
  String? error;
  Map<String, String> fieldErrors = const {};

  Future<void> submit(Future<void> Function() action) async {
    if (busy) return;
    setState(() {
      busy = true;
      error = null;
      fieldErrors = const {};
    });
    try {
      await action();
    } on ApiException catch (e) {
      if (mounted) {
        setState(() {
        error = e.message;
        fieldErrors = e.fields;
      });
      }
    } catch (_) {
      if (mounted) setState(() => error = 'Something went wrong. Please try again.');
    } finally {
      if (mounted) setState(() => busy = false);
    }
  }

  Widget errorBanner() => error == null
      ? const SizedBox.shrink()
      : Padding(
          padding: const EdgeInsets.only(bottom: AppSpace.md),
          child: Container(
            key: const Key('form.error'),
            padding: const EdgeInsets.all(AppSpace.md),
            decoration: BoxDecoration(color: AppColors.dangerBg, borderRadius: BorderRadius.circular(AppRadius.control)),
            child: Text(error!, style: AppText.bodyMd.copyWith(color: AppColors.danger)),
          ),
        );

  Widget submitButton(String label, VoidCallback onPressed, {Key? key}) => FilledButton(
        key: key,
        onPressed: busy ? null : onPressed,
        child: busy ? const SizedBox(width: 20, height: 20, child: CircularProgressIndicator(strokeWidth: 2, color: Colors.white)) : Text(label),
      );
}

class LoginScreen extends ConsumerStatefulWidget {
  const LoginScreen({super.key, this.message});
  final String? message;
  @override
  ConsumerState<LoginScreen> createState() => _LoginScreenState();
}

class _LoginScreenState extends ConsumerState<LoginScreen> with _Submitting {
  final _form = GlobalKey<FormState>();
  final _phone = TextEditingController();
  final _password = TextEditingController();

  @override
  void initState() {
    super.initState();
    error = widget.message;
  }

  @override
  Widget build(BuildContext context) => _AuthScaffold(
        title: 'Welcome back',
        subtitle: 'Sign in to your wallet',
        children: [
          errorBanner(),
          Form(
            key: _form,
            child: Column(children: [
              TextFormField(
                key: const Key('login.phone'),
                controller: _phone,
                keyboardType: TextInputType.phone,
                autofillHints: const [AutofillHints.telephoneNumber],
                decoration: const InputDecoration(labelText: 'Phone number', hintText: '07XX XXX XXX'),
                validator: validateKenyanPhone,
              ),
              const SizedBox(height: AppSpace.md),
              TextFormField(
                key: const Key('login.password'),
                controller: _password,
                obscureText: true,
                autofillHints: const [AutofillHints.password],
                decoration: const InputDecoration(labelText: 'Password'),
                validator: (v) => _required(v, 'password'),
              ),
            ]),
          ),
          Align(alignment: Alignment.centerRight, child: TextButton(onPressed: () => context.push('/forgot-password'), child: const Text('Forgot password?'))),
          const SizedBox(height: AppSpace.sm),
          submitButton('Sign in', key: const Key('login.submit'), () {
            if (!_form.currentState!.validate()) return;
            submit(() => ref.read(sessionProvider.notifier).login(_phone.text.trim(), _password.text));
          }),
          const SizedBox(height: AppSpace.md),
          TextButton(onPressed: () => context.go('/register'), child: const Text('New here? Create an account')),
        ],
      );
}

class RegisterScreen extends ConsumerStatefulWidget {
  const RegisterScreen({super.key});
  @override
  ConsumerState<RegisterScreen> createState() => _RegisterScreenState();
}

class _RegisterScreenState extends ConsumerState<RegisterScreen> with _Submitting {
  final _form = GlobalKey<FormState>();
  final _name = TextEditingController();
  final _phone = TextEditingController();
  final _email = TextEditingController();
  final _password = TextEditingController();

  @override
  Widget build(BuildContext context) => _AuthScaffold(
        title: 'Open your wallet',
        subtitle: 'It takes a minute. We will verify your phone number.',
        children: [
          errorBanner(),
          Form(
            key: _form,
            child: Column(children: [
              TextFormField(
                controller: _name,
                textCapitalization: TextCapitalization.words,
                decoration: InputDecoration(labelText: 'Full name', errorText: fieldErrors['full_name']),
                validator: (v) => (v ?? '').trim().length < 2 ? 'Enter your full name' : null,
              ),
              const SizedBox(height: AppSpace.md),
              TextFormField(
                controller: _phone,
                keyboardType: TextInputType.phone,
                decoration: InputDecoration(labelText: 'Phone number', hintText: '07XX XXX XXX', errorText: fieldErrors['phone']),
                validator: validateKenyanPhone,
              ),
              const SizedBox(height: AppSpace.md),
              TextFormField(
                controller: _email,
                keyboardType: TextInputType.emailAddress,
                decoration: InputDecoration(labelText: 'Email (optional)', errorText: fieldErrors['email']),
              ),
              const SizedBox(height: AppSpace.md),
              TextFormField(
                controller: _password,
                obscureText: true,
                decoration: InputDecoration(labelText: 'Password', helperText: 'At least 10 characters', errorText: fieldErrors['password']),
                validator: (v) => (v ?? '').length < 10 ? 'Use at least 10 characters' : null,
              ),
            ]),
          ),
          const SizedBox(height: AppSpace.lg),
          submitButton('Create account', () {
            if (!_form.currentState!.validate()) return;
            submit(() => ref.read(sessionProvider.notifier).register(
                phone: _phone.text.trim(), fullName: _name.text.trim(), password: _password.text, email: _email.text.trim()));
          }),
          const SizedBox(height: AppSpace.md),
          TextButton(onPressed: () => context.go('/login'), child: const Text('Already have an account? Sign in')),
        ],
      );
}

class VerifyPhoneScreen extends ConsumerStatefulWidget {
  const VerifyPhoneScreen({super.key});
  @override
  ConsumerState<VerifyPhoneScreen> createState() => _VerifyPhoneScreenState();
}

class _VerifyPhoneScreenState extends ConsumerState<VerifyPhoneScreen> with _Submitting {
  final _code = TextEditingController();

  @override
  Widget build(BuildContext context) {
    final user = ref.watch(currentUserProvider);
    return _AuthScaffold(
      title: 'Verify your phone',
      subtitle: 'Enter the 6-digit code sent to ${user?.phone ?? 'your phone'}.',
      children: [
        errorBanner(),
        TextField(
          controller: _code,
          keyboardType: TextInputType.number,
          maxLength: 6,
          inputFormatters: [FilteringTextInputFormatter.digitsOnly],
          style: AppText.headlineMd.copyWith(letterSpacing: 8, fontFeatures: tabular),
          textAlign: TextAlign.center,
          decoration: const InputDecoration(counterText: '', hintText: '••••••'),
        ),
        const SizedBox(height: AppSpace.lg),
        submitButton('Verify', () {
          if (_code.text.length != 6) {
            setState(() => error = 'Enter the 6-digit code');
            return;
          }
          submit(() => ref.read(sessionProvider.notifier).verifyPhone(_code.text));
        }),
        TextButton(
          onPressed: busy
              ? null
              : () => submit(() async {
                    await ref.read(authRepositoryProvider).resendCode();
                    if (context.mounted) showSnack(context, 'A new code is on its way');
                  }),
          child: const Text('Resend code'),
        ),
        TextButton(onPressed: () => ref.read(sessionProvider.notifier).logout(), child: const Text('Sign out')),
      ],
    );
  }
}

class SetPinScreen extends ConsumerStatefulWidget {
  const SetPinScreen({super.key, this.isChange = false});
  final bool isChange;
  @override
  ConsumerState<SetPinScreen> createState() => _SetPinScreenState();
}

class _SetPinScreenState extends ConsumerState<SetPinScreen> with _Submitting {
  final _form = GlobalKey<FormState>();
  final _password = TextEditingController();
  final _pin = TextEditingController();
  final _confirm = TextEditingController();

  @override
  Widget build(BuildContext context) => _AuthScaffold(
        title: widget.isChange ? 'Change transaction PIN' : 'Create your transaction PIN',
        subtitle: 'Your PIN authorizes transfers and withdrawals. Never share it - we will never ask for it.',
        children: [
          errorBanner(),
          Form(
            key: _form,
            child: Column(children: [
              TextFormField(controller: _password, obscureText: true, decoration: const InputDecoration(labelText: 'Account password'),
                  validator: (v) => _required(v, 'password')),
              const SizedBox(height: AppSpace.md),
              TextFormField(
                controller: _pin,
                obscureText: true,
                keyboardType: TextInputType.number,
                maxLength: 6,
                inputFormatters: [FilteringTextInputFormatter.digitsOnly],
                decoration: InputDecoration(labelText: 'New PIN (4-6 digits)', counterText: '', errorText: fieldErrors['pin']),
                validator: (v) => (v ?? '').length < 4 ? 'Use 4-6 digits' : null,
              ),
              const SizedBox(height: AppSpace.md),
              TextFormField(
                controller: _confirm,
                obscureText: true,
                keyboardType: TextInputType.number,
                maxLength: 6,
                inputFormatters: [FilteringTextInputFormatter.digitsOnly],
                decoration: const InputDecoration(labelText: 'Confirm PIN', counterText: ''),
                validator: (v) => v != _pin.text ? 'PINs do not match' : null,
              ),
            ]),
          ),
          const SizedBox(height: AppSpace.lg),
          submitButton('Save PIN', () {
            if (!_form.currentState!.validate()) return;
            submit(() async {
              await ref.read(sessionProvider.notifier).setPin(_password.text, _pin.text);
              if (widget.isChange && context.mounted) {
                showSnack(context, 'PIN updated');
                context.pop();
              }
            });
          }),
        ],
      );
}

class ForgotPasswordScreen extends ConsumerStatefulWidget {
  const ForgotPasswordScreen({super.key});
  @override
  ConsumerState<ForgotPasswordScreen> createState() => _ForgotPasswordScreenState();
}

class _ForgotPasswordScreenState extends ConsumerState<ForgotPasswordScreen> with _Submitting {
  final _phone = TextEditingController();
  final _code = TextEditingController();
  final _password = TextEditingController();
  bool _sent = false;

  @override
  Widget build(BuildContext context) => _AuthScaffold(
        title: 'Reset password',
        subtitle: _sent ? 'If the number is registered, we sent a 6-digit code.' : 'We will send a reset code to your phone.',
        children: [
          errorBanner(),
          TextField(controller: _phone, enabled: !_sent, keyboardType: TextInputType.phone, decoration: const InputDecoration(labelText: 'Phone number')),
          if (_sent) ...[
            const SizedBox(height: AppSpace.md),
            TextField(controller: _code, keyboardType: TextInputType.number, decoration: const InputDecoration(labelText: 'Code')),
            const SizedBox(height: AppSpace.md),
            TextField(controller: _password, obscureText: true, decoration: const InputDecoration(labelText: 'New password (min 10 characters)')),
          ],
          const SizedBox(height: AppSpace.lg),
          submitButton(_sent ? 'Reset password' : 'Send code', () {
            if (validateKenyanPhone(_phone.text) != null) {
              setState(() => error = 'Enter a valid Kenyan mobile number');
              return;
            }
            submit(() async {
              final repo = ref.read(authRepositoryProvider);
              if (!_sent) {
                await repo.forgotPassword(_phone.text.trim());
                setState(() => _sent = true);
              } else {
                await repo.resetPassword(_phone.text.trim(), _code.text.trim(), _password.text);
                if (context.mounted) {
                  showSnack(context, 'Password reset. Please sign in.');
                  context.go('/login');
                }
              }
            });
          }),
        ],
      );
}
