import { hash } from 'rsvp';
import Base from '../../replication-base';

export default Base.extend({
  modelPath: 'model.config', // TODO (unload mixin): when removing mixin, remove prepended 'model'

  model(params) {
    return hash({
      cluster: this.modelFor('mode.secondaries'),
      config: this.store.findRecord('path-filter-config', params.secondary_id),
    });
  },

  redirect(model) {
    const cluster = model.cluster;
    let replicationMode = this.paramsFor('mode').replication_mode;
    if (
      !this.version.hasPerfReplication ||
      replicationMode !== 'performance' ||
      !cluster.get(`${replicationMode}.isPrimary`) ||
      !cluster.get('canAddSecondary')
    ) {
      return this.transitionTo('mode', replicationMode);
    }
  },
});
