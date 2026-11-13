import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../app/providers.dart';
import '../../../core/networking/api_error.dart';
import '../domain/user.dart';

sealed class SessionState {
  const SessionState();
}

class SessionLoading extends SessionState {
  const SessionLoading();
}

class SignedOut extends SessionState {
  const SignedOut([this.message]);
  final String? message;
}

class SignedIn extends SessionState {
  const SignedIn(this.user);
  final AppUser user;
}

final sessionProvider = NotifierProvider<SessionController, SessionState>(SessionController.new);

/// The signed-in user; rebuilds dependents whenever the session changes.
final currentUserProvider = Provider<AppUser?>((ref) => switch (ref.watch(sessionProvider)) {
      SignedIn(:final user) => user,
      _ => null,
    });

class SessionController extends Notifier<SessionState> {
  @override
  SessionState build() {
    ref.read(apiClientProvider).onSessionExpired = expire;
    Future.microtask(restore);
    return const SessionLoading();
  }

  AppUser? get user => switch (state) { SignedIn(:final user) => user, _ => null };

  Future<void> restore() async {
    try {
      final u = await ref.read(authRepositoryProvider).restore();
      state = u == null ? const SignedOut() : SignedIn(u);
    } on ApiException catch (e) {
      // Offline: keep tokens but show sign-in; an expired session clears them.
      state = SignedOut(e.code == 'network_error' ? e.message : null);
    }
  }

  Future<void> login(String phone, String password) async {
    state = SignedIn(await ref.read(authRepositoryProvider).login(phone, password));
  }

  Future<void> register({required String phone, required String fullName, required String password, String? email}) async {
    state = SignedIn(await ref.read(authRepositoryProvider).register(phone: phone, fullName: fullName, password: password, email: email));
  }

  Future<void> verifyPhone(String code) async {
    state = SignedIn(await ref.read(authRepositoryProvider).verifyPhone(code));
  }

  Future<void> setPin(String password, String pin) async {
    await ref.read(authRepositoryProvider).setPin(password, pin);
    await refreshUser();
  }

  Future<void> refreshUser() async {
    state = SignedIn(await ref.read(authRepositoryProvider).me());
  }

  Future<void> logout() async {
    try {
      await ref.read(authRepositoryProvider).logout();
    } catch (_) {
      // Tokens are cleared locally regardless.
    }
    state = const SignedOut();
  }

  void expire() => state = const SignedOut('Your session expired. Please sign in again.');
}
