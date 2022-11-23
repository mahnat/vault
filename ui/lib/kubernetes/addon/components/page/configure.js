import Component from '@glimmer/component';
import { inject as service } from '@ember/service';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';
import { task } from 'ember-concurrency';
import { waitFor } from '@ember/test-waiters';
import errorMessage from 'vault/utils/error-message';

export default class ConfigurePageComponent extends Component {
  @service router;
  @service store;

  @tracked inferredState;
  @tracked modelValidations;
  @tracked error;
  @tracked showConfirm;

  constructor() {
    super(...arguments);
    if (!this.args.model.isNew && !this.args.model.disableLocalCaJwt) {
      this.inferredState = 'success';
    }
  }

  get isDisabled() {
    if (!this.args.model.disableLocalCaJwt && this.inferredState !== 'success') {
      return true;
    }
    return this.save.isRunning || this.fetchInferred.isRunning;
  }

  leave(route) {
    this.router.transitionTo(`vault.cluster.secrets.backend.kubernetes.${route}`);
  }

  @action
  onRadioSelect(value) {
    this.args.model.disableLocalCaJwt = value;
    this.inferredState = null;
  }

  @task
  @waitFor
  *fetchInferred() {
    try {
      yield this.store.adapterFor('kubernetes/config').checkConfigVars(this.args.model.backend);
      this.inferredState = 'success';
    } catch {
      this.inferredState = 'error';
    }
  }

  @task
  @waitFor
  *save() {
    if (!this.args.model.isNew && !this.showConfirm) {
      this.showConfirm = true;
      return;
    }
    this.showConfirm = false;
    try {
      yield this.args.model.save();
      this.leave('configuration');
    } catch (error) {
      this.error = errorMessage(error, 'Error saving configuration. Please try again or contact support');
    }
  }

  @action
  cancel() {
    const { model } = this.args;
    const transitionRoute = model.isNew ? 'overview' : 'configuration';
    const cleanupMethod = model.isNew ? 'unloadRecord' : 'rollbackAttributes';
    model[cleanupMethod]();
    this.leave(transitionRoute);
  }
}
